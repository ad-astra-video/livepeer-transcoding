package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/daos"
	"github.com/pocketbase/pocketbase/models"
	ffmpeg "github.com/u2takey/ffmpeg-go"
)

type TranscodeFile struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	AuthID   string `json:"accessKeyId"`
	AuthPW   string `json:"secretAccessKey"`
	Bucket   string `json:"bucket"`
	Path     string `json:"path"`
}

type TranscodeOutput struct {
	Type string
	Path string
}

type Profile struct {
	Name         string `json:"name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Encoder      string `json:"encoder"`
	Bitrate      int    `json:"bitrate,omitempty"`
	FPS          int    `json:"fps,omitempty"`
	FPSDen       int    `json:"fpsDen,omitempty"`
	Profile      string `json:"profile,omitempty"`
	GOP          string `json:"gop,omitempty"`
	ColorDepth   int    `json:"colorDepth,omitempty"`
	ChromaFormat string `json:"chromaFormat,omitempty"`
	Quality      int    `json:"quality,omitempty"`
}

type TranscodeRequest struct {
	Input               TranscodeFile     `json:"input"`
	Storage             TranscodeFile     `json:"storage"`
	Output              []TranscodeOutput `json:"outputs"`
	Profiles            []Profile         `json:"profiles"`
	ParallelTranscoding bool              `json:"parallel_transcoding"`
}

type Broadcaster struct {
	Url      *url.URL
	User     string
	Password string
}

type FfmpegTranscode struct {
	WorkDir      string
	UploadFile   string
	TargetSegDur int
	ManifestID   string
	Broadcasters []*Broadcaster
	Request      TranscodeRequest
	RequestId    string
	User         *models.Record
	pApp         *pocketbase.PocketBase
}

func NewFfmpegTranscode(workDir string, req string, broadcasters []*Broadcaster, user *models.Record, app *pocketbase.PocketBase) (*FfmpegTranscode, error) {
	var transcodeReq TranscodeRequest
	err := json.Unmarshal([]byte(req), &transcodeReq)
	if err != nil {
		err = errors.New(fmt.Sprintf("could not parse request: %v\n", err.Error()))
		ErrorLogger.Println(err.Error())
		return nil, err
	}

	return &FfmpegTranscode{
		WorkDir:      workDir,
		UploadFile:   "",
		Request:      transcodeReq,
		ManifestID:   uuid.NewString(),
		Broadcasters: broadcasters,
		User:         user,
		pApp:         app,
		TargetSegDur: 10}, nil
}

func (f *FfmpegTranscode) StartTranscode() {
	//save transcode request to db
	tRecord, tErr := f.saveTranscodeReq()
	f.RequestId = tRecord.Id
	if tErr != nil {
		ErrorLogger.Printf("%v\n", tErr.Error())
		f.transcodeFailed(tRecord, tErr)
		return
	}
	//get the file if s3
	if f.Request.Input.Type == "s3" {
		f.updateTranscodeReqStatus(tRecord, "queued", "downloading s3 file")

		fp, fpErr := f.newS3UploadFile()
		f.UploadFile = fp
		if fp != "" {
			ErrorLogger.Println(fmt.Sprint("could not create file for upload: %w", fpErr))
			f.transcodeFailed(tRecord, fpErr)
			return
		}
		dErr := f.downloadVideo()
		if dErr != nil {
			ErrorLogger.Printf("could not download file: %v\n", dErr.Error())
			f.transcodeFailed(tRecord, dErr)
			return
		}
	} else {
		//file uploaded to server for transcoding, get local filename from database and filetype
		uploadFile, err := f.pApp.Dao().FindRecordsByFilter("uploads", "filename ~ {:filename} && user={:userid}", "", 1, 0, dbx.Params{"filename": f.Request.Input.Path, "userid": f.User.Id})
		if uploadFile[0].GetBool("complete") == false {
			//TODO: add to queue
			InfoLogger.Printf("could not start transcode, file upload not complete")
			f.updateTranscodeReqStatus(tRecord, "queued", "transcode will start when upload is complete")
		}
		if err != nil {
			ErrorLogger.Printf("could not start transcode, local file not found  %v\n", err.Error())
			f.transcodeFailed(tRecord, err)
			return
		}

		f.Request.Input.Type = uploadFile[0].GetString("filetype")
		f.UploadFile = uploadFile[0].GetString("localfile")
	}

	if f.Request.ParallelTranscoding {
		f.updateTranscodeReqStatus(tRecord, "in_progress", "segmenting video")
		err := f.segmentAndTranscodeVideo(f.TargetSegDur)
		if err != nil {
			ErrorLogger.Printf("error transcoding: %v\n", err.Error())
			f.transcodeFailed(tRecord, err)
			return
		}

		f.transcodeComplete(tRecord)

	} else {
		//this would be for transcoding the entire file on one CPU.
		f.updateTranscodeReqStatus(tRecord, "in_progress", "sending file to transcode")

		f.transcodeComplete(tRecord)
	}
}

func (f *FfmpegTranscode) segmentAndTranscodeVideo(segDur int) error {
	inpExt := path.Ext(f.UploadFile)
	if inpExt == "" {
		inpExt = extFromFileType(f.Request.Input.Type)
	}
	fn := f.WorkDir + "/" + strings.ReplaceAll(path.Base(f.UploadFile), inpExt, "")
	if fn == "." || fn == "/" {
		return errors.New("invalid file provided")
	}
	seg_list := fn + ".csv"
	//remove seg list if exists
	_, err := os.Stat(seg_list)
	if err == nil {
		err = os.Remove(seg_list)
		if err != nil {
			ErrorLogger.Printf("video segment had error, could not delete segment list: %v\n", err.Error())
			return err
		}
	}

	//segment video on key frames using
	fpErr := ffmpeg.Input(f.UploadFile, ffmpeg.KwArgs{"f": strings.Replace(inpExt, ".", "", 1)}).Output(fn+"_%d"+inpExt, ffmpeg.KwArgs{"f": "segment", "segment_time": fmt.Sprint(segDur), "min_seg_duration": fmt.Sprint(segDur / 2), "segment_list": seg_list, "segment_list_type": "csv", "reset_timestamps": "0", "c": "copy"}).OverWriteOutput().ErrorToStdOut().Run()

	if fpErr != nil {
		ErrorLogger.Printf("video segmenter had error:  %v\n", err.Error())
	}

	//add segments to db for tracking
	pErr := f.processSegmentList(seg_list)
	if pErr != nil {
		return pErr
	}
	//start transocding
	return f.transcodeSegments()

}

func (f *FfmpegTranscode) processSegmentList(seg_list string) error {
	InfoLogger.Printf(f.RequestId + " processing segment list")

	segSaveErr := errors.New("could not create segment record")
	//process seg list
	segList, err := os.Open(seg_list)
	defer segList.Close()
	if err != nil {
		return errors.New("could not open segment list")
	}
	fs := bufio.NewReader(segList)

	txErr := f.pApp.Dao().RunInTransaction(func(txDao *daos.Dao) error {
		segments, scErr := txDao.FindCollectionByNameOrId("segments")
		if scErr != nil {
			ErrorLogger.Printf("could not get transcodes or segments collection: %v\n", scErr)
			return segSaveErr
		}
		//read lines and add segments to DB
		segCnt := 1
		for {
			line, _, err := fs.ReadLine()
			if err != nil {
				break
			}
			if len(line) > 0 {
				//parse the segment info
				segInfo := strings.Split(string(line), ",")
				file := segInfo[0]
				start, sErr := strconv.ParseFloat(segInfo[1], 64)
				end, eErr := strconv.ParseFloat(segInfo[2], 64)
				if sErr != nil || eErr != nil {
					start = float64(0)
					end = float64(10)
				}

				//create record and save it for segments
				record := models.NewRecord(segments)
				record.Set("segfile", f.WorkDir+"/"+file)
				record.Set("start", start)
				record.Set("end", end)
				record.Set("failures", 0)
				record.Set("transcode", f.RequestId)
				record.Set("status", "queued")
				record.Set("num", segCnt)
				if err := txDao.SaveRecord(record); err != nil {
					ErrorLogger.Printf("error saving segment: %v\n", err.Error())
					return segSaveErr
				}
				//increase cnt for segments
				segCnt++
			}
		}

		return nil
	})

	return txErr
}

func (f *FfmpegTranscode) transcodeSegments() error {
	InfoLogger.Printf(f.RequestId + " transcoding segments")

	//get all segments for transcode
	segments, sgErr := f.pApp.Dao().FindRecordsByFilter("segments", "transcode = {:tid}", "", 0, 0, dbx.Params{"tid": f.RequestId})
	if sgErr != nil {
		return errors.New("could not get segments for transcode")
	}

	//transcode up to 5 segments at one time
	segPace := float64(2)
	tchan := make(chan int, 5)
	var wg sync.WaitGroup
	InfoLogger.Printf("%v transcoding %v segments\n", f.RequestId, len(segments))
	for ss := 0; ss < 3; ss++ {
		//try transcode 3 times
		for _, seg := range segments {
			if seg.GetString("status") == "complete" {
				InfoLogger.Printf("%v skipping segment %v, transcoding complete", seg.GetString("transcode"), seg.GetString("num"))
				continue
			}
			//limit transcodes to channel size and wait for all to complete before exiting
			wg.Add(1)
			tchan <- 1
			go func(seg *models.Record) {
				//try 5 times to transcode segments
				maxRetries := 5
				baseDelay := 15
				for tt := 1; tt <= maxRetries; tt++ {
					InfoLogger.Printf("%v segment %v transcode attempt %v", f.RequestId, seg.GetString("num"), seg.GetInt("failures")+1)
					start := time.Now()
					err := f.sendTranscode(seg)

					if err == nil {
						if time.Since(start) <= (15 * time.Second) {
							segPace--
						}

						<-tchan
						break
					} else {
						segPace++
						cd := time.Duration(tt*2*baseDelay) * time.Second
						InfoLogger.Printf("%v  segment %v did not complete, waiting %v\n", f.RequestId, seg.GetString("num"), cd.Seconds())
						//wait for cooldown
						time.Sleep(cd)
					}
				}

				//transcode complete for segment (may be error but tried 5 times)
				wg.Done()
				<-tchan

			}(seg)

			//pace submitting segments
			p := time.Duration(math.Max(2, segPace)) * time.Second
			InfoLogger.Printf("sending next segment after %v seconds", p.Seconds())
			time.Sleep(p)
		}
	}

	wg.Wait()

	return nil
}

// CustomReader is a wrapper for the underlying data source with a larger buffer size.
type CustomReader struct {
	underlyingReader io.Reader
	bufferSize       int
}

func (r *CustomReader) Read(p []byte) (n int, err error) {
	return r.underlyingReader.Read(p)
}

func (f *FfmpegTranscode) sendTranscode(segment *models.Record) error {

	//update segment status
	f.updateSegmentTranscodeStatus(segment, "in_progress", "transcoding")

	segFile := segment.GetString("segfile")
	start := segment.GetFloat("start")
	end := segment.GetFloat("end")
	num := segment.GetString("num")
	segDur := (end - start) * float64(1000)
	transcodeConfig, tcErr := f.createTranscodeConfig()

	if tcErr != nil {
		return f.segmentTranscodeFailed(segment, errors.New("failed to parse transcode config"))
	}

	//test opening file
	InfoLogger.Printf("%v opening semgent to send: %v\n", f.RequestId, segFile)
	segF, sfErr := os.Open(segFile)
	defer segF.Close()
	if sfErr != nil {
		ErrorLogger.Printf("%v segment open error %v\n", f.RequestId, sfErr.Error())
		return f.segmentTranscodeFailed(segment, errors.New("failed to open input file"))
	}
	segData, _ := io.ReadAll(segF)
	InfoLogger.Printf("%v transcoding segment %v", f.RequestId, segFile)

	for _, b := range f.Broadcasters {
		bUrl := b.Url.String() + "/" + f.ManifestID + "/" + num + path.Ext(segFile)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(f.TargetSegDur*20)*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", bUrl, bytes.NewBuffer(segData))
		if b.User != "" {
			req.SetBasicAuth(b.User, b.Password)
		}
		req.Header.Add("Accept", "multipart/mixed")
		req.Header.Add("Content-Duration", fmt.Sprintf("%d", int(segDur)))
		req.Header.Add("Content-Resolution", "1920x1080") //TODO: consider getting this. B should start parsing this but it does not go into fee paid
		req.Header.Add("Livepeer-Transcode-Configuration", transcodeConfig)

		resp, rErr := http.DefaultClient.Do(req)
		if rErr != nil {
			ErrorLogger.Printf("%v failed to send request to transcode: %v\n", f.RequestId, rErr.Error())
			continue //try another broadcaster
		}
		defer resp.Body.Close()

		//if http error from B move to next
		if resp.StatusCode != 200 {
			respBody, _ := io.ReadAll(resp.Body)
			ErrorLogger.Printf("%v failed to send transcode %v %v to %v", f.RequestId, resp.StatusCode, string(respBody), bUrl)
			//time.Sleep(1 * time.Second)
			continue
		}

		if resp.StatusCode == 200 {
			mediaType, params, mErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			//TODO: responses were blank if not including multipart/mixed header.
			//      should have header of application/vnd+livepeer.uri
			if mErr != nil || mediaType != "multipart/mixed" {
				return f.segmentTranscodeFailed(segment, errors.New("response header invalid"))
			}

			//debug - save response
			//data, rErr := io.ReadAll(resp.Body)
			//os.WriteFile(f.WorkDir+"/resp.body", data, 0644)
			//os.WriteFile(f.WorkDir+"/resp.header", []byte(fmt.Sprintf("%+v", resp.Header)), 0644)

			if rErr != nil {
				return f.segmentTranscodeFailed(segment, errors.New("could not read response"))
			}

			mr := multipart.NewReader(resp.Body, params["boundary"])

			for {
				part, err := mr.NextPart()

				if err == io.EOF {
					break
				}
				if err != nil {
					return f.segmentTranscodeFailed(segment, errors.New(fmt.Sprintf("multipart reponse parsing error (could not read part, %v)", err.Error())))
				}
				fn := part.FileName()
				fn = strings.ReplaceAll(fn, "/", "")
				fn = strings.ReplaceAll(fn, "..", "")

				if fn == "" {
					return f.segmentTranscodeFailed(segment, errors.New("no filename returned with segment"))
				}

				// Create a file to save the part's content
				file, err := os.Create(f.WorkDir + "/" + segment.GetString("transcode") + "_" + part.FileName())
				if err != nil {
					return f.segmentTranscodeFailed(segment, errors.New(fmt.Sprintf("multipart reponse parsing error (could not create file for part data, %v)", err.Error())))
				}
				defer file.Close()

				// Copy the part's content to the file
				_, err = io.Copy(file, part)
				if err != nil {
					return f.segmentTranscodeFailed(segment, errors.New("multipart reponse parsing error (EOF)"))
				}

				f.segmentTranscodeComplete(segment)
				InfoLogger.Printf("%v segment %v transcoded, rendition %v saved\n", f.RequestId, segment.GetString("num"), part.FileName())

			}

			return nil
		}

	}

	return f.segmentTranscodeFailed(segment, errors.New("need to retry segment"))
}

func (f *FfmpegTranscode) createTranscodeConfig() (string, error) {
	config := make(map[string]interface{})
	config["manifestID"] = uuid.NewString()
	config["timeoutMultiplier"] = f.TargetSegDur * 100
	config["profiles"] = f.Request.Profiles

	configStr, cErr := json.Marshal(config)
	if cErr != nil {
		return "", cErr
	}

	return string(configStr), nil
}

func (f *FfmpegTranscode) segmentTranscodeFailed(segment *models.Record, segErr error) error {
	fails := segment.GetInt("failures")
	fails++
	segment.Set("status", "error")
	segment.Set("status_message", segErr.Error())
	segment.Set("failures", fails)
	sErr := f.pApp.Dao().SaveRecord(segment)
	if sErr != nil {
		ErrorLogger.Printf("%v segment %v could not update status\n", f.RequestId, segment.Id)
	}

	ErrorLogger.Printf("%v segment %v transcode failed: %v\n", f.RequestId, segment.GetString("num"), segErr.Error())
	return segErr
}

func (f *FfmpegTranscode) transcodeFailed(req *models.Record, reqErr error) error {
	fails := req.GetInt("failures")
	fails++
	req.Set("status", "error")
	req.Set("status_message", reqErr.Error())
	req.Set("failures", fails)
	sErr := f.pApp.Dao().SaveRecord(req)
	if sErr != nil {
		ErrorLogger.Printf("%v trancode could not update status\n", req.Id)
	}

	ErrorLogger.Printf("%v transcode failed: %v\n", req.Id, reqErr.Error())
	return reqErr
}

func (f *FfmpegTranscode) updateSegmentTranscodeStatus(segment *models.Record, status string, message string) {
	segment.Set("status", status)
	segment.Set("status_message", "transcoding")
	sErr := f.pApp.Dao().SaveRecord(segment)
	if sErr != nil {
		ErrorLogger.Printf("%v segment %v could not update status\n", f.RequestId, segment.Id)
	}
}

func (f *FfmpegTranscode) segmentTranscodeComplete(segment *models.Record) {
	segment.Set("status", "complete")
	segment.Set("status_message", "complete")
	sErr := f.pApp.Dao().SaveRecord(segment)
	if sErr != nil {
		ErrorLogger.Printf("%v segment %v could not update status\n", f.RequestId, segment.Id)
	}
}

func (f *FfmpegTranscode) updateTranscodeReqStatus(req *models.Record, status string, message string) {
	req.Set("status", status)
	req.Set("status_message", message)
	err := f.pApp.Dao().SaveRecord(req)
	if err != nil {
		ErrorLogger.Printf("%v failed to save status update  error: %v\n", req.Id, err.Error())
	}
}

func (f *FfmpegTranscode) transcodeComplete(req *models.Record) {
	req.Set("status", "complete")
	req.Set("status_message", "complete")
	err := f.pApp.Dao().SaveRecord(req)
	if err != nil {
		ErrorLogger.Printf("%v failed to save status update  error: %v\n", req.Id, err.Error())
	}
}

func (f *FfmpegTranscode) saveTranscodeReq() (*models.Record, error) {
	tSaveErr := errors.New("transcode failed: could not create record")
	collection, err := f.pApp.Dao().FindCollectionByNameOrId("transcodes")
	if err != nil {
		return nil, tSaveErr
	}
	tReq, _ := json.Marshal(f.Request)
	record := models.NewRecord(collection)
	record.Set("filename", f.Request.Input.Path)
	record.Set("request", string(tReq))
	record.Set("status", "queued")
	record.Set("failures", 0)
	record.Set("user", f.User.Id)
	if err := f.pApp.Dao().SaveRecord(record); err != nil {
		fmt.Printf("error saving transcode request: %v\n", err.Error())
		return nil, tSaveErr
	} else {
		return record, nil
	}
}

func (f *FfmpegTranscode) newS3UploadFile() (string, error) {
	collection, err := f.pApp.Dao().FindCollectionByNameOrId("uploads")
	if err != nil {
		return "", err
	}
	fileType, err := mimetype.DetectFile(f.Request.Input.Path)
	if err != nil {
		return "", errors.New("could not detect file type, make sure file has appropriate extension")
	} else {
		if strings.Contains(fileType.String(), "video") == false {
			return "", errors.New("file is not video, make sure file is video")
		}
	}

	record := models.NewRecord(collection)
	record.Set("user", f.User.Id)
	record.Set("localfile", f.pApp.DataDir()+"/videos/uploads/"+uuid.NewString()+fileType.Extension())
	record.Set("filename", f.Request.Input.Path)
	record.Set("filetype", fileType.String())

	if err := f.pApp.Dao().SaveRecord(record); err != nil {
		fmt.Printf("%v\n", err.Error())
		return "", err
	} else {
		return record.GetString("localfile"), nil
	}

}

func (f *FfmpegTranscode) downloadVideo() error {
	_, err := url.Parse(f.Request.Input.Endpoint)
	if err != nil {
		fmt.Println("failed to parse endpoint url")
		return errors.New("failed to download video: failed to parse s3 url. make sure is like https://endpoint.s3.url")
	}

	s3Client, err := minio.New(f.Request.Input.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(f.Request.Input.AuthID, f.Request.Input.AuthPW, ""),
		Secure: true,
	})

	if err != nil {
		fmt.Println("failed to download video, s3 connection failed")
		return errors.New("failed to download video: s3 connection failed")
	}

	reader, err := s3Client.GetObject(context.Background(), f.Request.Input.Bucket, f.Request.Input.Path, minio.GetObjectOptions{})
	if err != nil {
		return errors.New("failed to download video: could not get object path")
	}
	defer reader.Close()

	localFile, err := os.Create(f.UploadFile)
	if err != nil {
		return errors.New("failed to download video: could not create local file")
	}
	defer localFile.Close()

	stat, err := reader.Stat()
	if err != nil {
		return errors.New("failed to download video: could not stat local file")
	}

	if _, err := io.CopyN(localFile, reader, stat.Size); err != nil {
		return errors.New("failed to download video: failed copying s3 download to file")
	}

	//file downloaded successfully
	return nil
}

func checkTranscodeRequests(app *pocketbase.PocketBase) {

	transcodes, err := app.Dao().FindRecordsByFilter("transcodes", "status = 'queued' && failures < 10", "+created", 20, 0, dbx.Params{})
	if err != nil {
		ErrorLogger.Printf("could not get queued transcodes: %v", err.Error())
		return
	}

	broadcasters, bErr := getBroadcasters(app.DataDir())
	if bErr != nil {
		ErrorLogger.Printf("could not get broadcasters for processing queued transcodes: %v", bErr.Error())
		return
	}

	for _, t := range transcodes {
		t_user := t.ExpandedOne("user")
		nt, ntErr := NewFfmpegTranscode(app.DataDir(), t.GetString("request"), broadcasters, t_user, app)
		if err != nil {
			ErrorLogger.Printf("could not start transcode for %v for user %v: %v", t.GetString("filename"), t_user.Username(), ntErr.Error())
			continue
		}

		go nt.StartTranscode()

	}
}

func (f *FfmpegTranscode) getFileInfo() map[string]any {
	data, err := ffmpeg.Probe(f.UploadFile, nil)
	if err != nil {
		return nil
	}
	pd := make(map[string]any)
	err = json.Unmarshal([]byte(data), &pd)
	if err != nil {
		return nil
	}

	return pd
}

func extFromFileType(ft string) string {
	switch ft {
	case "video/mp4":
		return ".mp4"
	case "video/MP2T":
		return ".ts"
	case "video/webm":
		return ".webm"
	case "video/x-matroska":
		return ".mkv"
	default:
		return ".mp4"
	}
}
