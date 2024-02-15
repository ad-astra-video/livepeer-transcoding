package main

import (
	"bufio"
	b64 "encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/labstack/echo/v5"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/tokens"
	"github.com/pocketbase/pocketbase/tools/cron"
	"github.com/pocketbase/pocketbase/tools/security"
	"github.com/pocketbase/pocketbase/tools/template"
	"github.com/spf13/cast"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

//"github.com/google/uuid"

var (
	WarningLogger *log.Logger
	InfoLogger    *log.Logger
	ErrorLogger   *log.Logger
)

func initLogger() {
	InfoLogger = log.New(os.Stderr, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	WarningLogger = log.New(os.Stderr, "WARNING: ", log.Ldate|log.Ltime|log.Lshortfile)
	ErrorLogger = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
}

func main() {

	initLogger()

	app := pocketbase.New()

	setupRoutes(app)

	setupTasks(app)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func setupTasks(app *pocketbase.PocketBase) {
	c := cron.New()
	//try and start transcodes that are in queded state
	c.Add("start_transcodes", "0/1 * 0 ? * * *", func() {
		checkTranscodeRequests(app)
	})
}

func setupRoutes(app *pocketbase.PocketBase) {
	handler, err := initTus(app)
	if err != nil {
		panic(fmt.Errorf("Unable to initialize tusd, make sure 'uploads' folder exists in datadir: %s", err))
	}
	go func() {
		for {
			event := <-handler.CompleteUploads
			InfoLogger.Printf("Upload %s finished\n", event.Upload.ID)
			//get upload record and mark complete
			uploadFile, err := app.Dao().FindRecordsByFilter("uploads", "localfile ~ {:uploadId}", "", 1, 0, dbx.Params{"uploadId": event.Upload.ID})
			if err != nil {
				ErrorLogger.Printf("error finding upload: %v\n", err.Error())
				continue
			}
			uploadFile[0].Set("complete", true)
			app.Dao().SaveRecord(uploadFile[0])

		}
	}()

	app.OnBeforeServe().Add(func(e *core.ServeEvent) error {
		registry := template.NewRegistry()
		e.Router.Use(loadAuthContextFromCookie(app))
		e.Router.Use(apis.ActivityLogger(app))

		//tus uploads endpoints
		e.Router.GET("/upload/*", echo.WrapHandler(http.StripPrefix("/upload/", handler)))
		e.Router.POST("/upload/*", echo.WrapHandler(http.StripPrefix("/upload/", handler)), saveTusUpload(app))
		e.Router.PATCH("/upload/*", echo.WrapHandler(http.StripPrefix("/upload/", handler)))
		e.Router.DELETE("/upload/*", echo.WrapHandler(http.StripPrefix("/upload/", handler)))
		e.Router.HEAD("/upload/*", echo.WrapHandler(http.StripPrefix("/upload/", handler)))

		e.Router.GET("/static/*", apis.StaticDirectoryHandler(os.DirFS(app.DataDir()+"/pb_public/"), false))

		e.Router.GET("/", func(c echo.Context) error {
			isGuest := c.Get("isGuest").(bool)
			if !isGuest {
				return c.Redirect(301, "/transcode")
			}

			html, err := registry.LoadFiles(
				app.DataDir()+"/pb_public/views/base.html",
				app.DataDir()+"/pb_public/views/login.html",
			).Render(nil)

			if err != nil {
				// or redirect to a dedicated 404 HTML page
				return apis.NewNotFoundError("", err)
			}

			return c.HTML(http.StatusOK, html)
		})

		e.Router.GET("/transcode", func(c echo.Context) error {
			isGuest := c.Get("isGuest").(bool)
			if isGuest {
				fmt.Println("redirecting from /transcode, not logged in")
				return c.Redirect(301, "/")
			}
			user, _ := c.Get(apis.ContextAuthRecordKey).(*models.Record)

			html, err := registry.LoadFiles(
				app.DataDir()+"/pb_public/views/base.html",
				app.DataDir()+"/pb_public/views/transcode.html",
				app.DataDir()+"/pb_public/views/profiles.html",
			).Render(nil)

			if err != nil {
				// or redirect to a dedicated 404 HTML page
				return apis.NewNotFoundError("", err)
			}

			c.SetCookie(createUserCookie(app, user))
			return c.HTML(http.StatusOK, html)
		})

		e.Router.GET("/settings", func(c echo.Context) error {
			isGuest := c.Get("isGuest").(bool)
			if isGuest {
				ErrorLogger.Println("redirecting from /settings, not logged in")
				return c.Redirect(301, "/")
			}
			user, _ := c.Get(apis.ContextAuthRecordKey).(*models.Record)

			html, err := registry.LoadFiles(
				app.DataDir()+"/pb_public/views/base.html",
				app.DataDir()+"/pb_public/views/settings.html",
				app.DataDir()+"/pb_public/views/profiles.html",
			).Render(nil)

			if err != nil {
				// or redirect to a dedicated 404 HTML page
				return apis.NewNotFoundError("", err)
			}

			c.SetCookie(createUserCookie(app, user))
			return c.HTML(http.StatusOK, html)

		})

		e.Router.POST("/register", func(c echo.Context) error {
			data := apis.RequestInfo(c).Data
			username := data["username"].(string)
			password := data["password"].(string) //limited to 20 chars below, if starts with 0x then added to ethSig field
			email := data["email"].(string)
			if email == "" {
				email = username
			}
			InfoLogger.Printf("registering %v %v\n", username, email)
			collection, err := app.Dao().FindCollectionByNameOrId("users")
			if err != nil {
				return err
			}

			record := models.NewRecord(collection)
			un_err := record.SetUsername(username)
			em_err := record.SetEmail(email)
			pw_err := record.SetPassword(password[0:min(20, len(password))])
			if password[0:2] == "0x" {
				if !verifySig(username, "TranscodeWithLivepeer", password) {
					return apis.NewBadRequestError("signature verification failed", nil)
				}
				record.Set("ethSig", password)

			}
			if un_err != nil || em_err != nil || pw_err != nil {
				fmt.Printf("%v\n", err.Error())
				return apis.NewApiError(500, "user not created, error with data", nil)
			} else {
				if err := app.Dao().SaveRecord(record); err != nil {
					ErrorLogger.Printf("%v\n", err.Error())
					return apis.NewApiError(500, "could not add user", nil)
				}
			}
			user, err := app.Dao().FindAuthRecordByUsername("users", username)
			if err == nil {
				//send back auth with cookie
				c.SetCookie(createUserCookie(app, user))
				return apis.RecordAuthResponse(app, c, user, nil)
			} else {
				return c.JSON(http.StatusOK, map[string]string{"message": "registration completed"})
			}
		})

		e.Router.POST("/login", func(c echo.Context) error {
			data := apis.RequestInfo(c).Data
			username := data["username"].(string)
			password := data["password"].(string)
			InfoLogger.Printf("checking login for:  %v\n", username)
			user, err := app.Dao().FindAuthRecordByUsername("users", username)

			if err != nil || !user.ValidatePassword(password[0:min(20, len(password))]) {
				InfoLogger.Printf("password failed: %v\n", user.Username())
				return apis.NewBadRequestError("Invalid credentials", err)
			} else {
				//check if full signature matches if eth sig
				if password[0:2] == "0x" {
					if user.Get("ethSig") != password {
						InfoLogger.Printf("eth sig did not match: %v\n", user.Username())
						return apis.NewBadRequestError("Invalid credentials", err)
					}
				}
				//send back auth with cookie
				c.SetCookie(createUserCookie(app, user))
				return apis.RecordAuthResponse(app, c, user, nil)
			}
		})

		e.Router.POST("/transcode", func(c echo.Context) error {
			user, _ := c.Get(apis.ContextAuthRecordKey).(*models.Record)
			if user == nil {
				return c.JSON(400, map[string]string{"message": "invalid user, auth token required"})
			}
			//load broadcasters
			bUrls, bErr := getBroadcasters(app.DataDir())
			if bErr != nil {
				return apis.NewApiError(500, "could not get broadcaster urls", nil)
			}

			data, dErr := io.ReadAll(c.Request().Body)
			if dErr != nil {
				ErrorLogger.Printf("could not start transcode, request data not valid: %v\n", err.Error())
				return apis.NewApiError(500, "could not start transcode", nil)
			}
			transcodeReq := string(data)
			t, err := NewFfmpegTranscode(app.DataDir()+"/segments", transcodeReq, bUrls, user, app)
			if err != nil {
				ErrorLogger.Printf("could not start transcode: %v\n", err.Error())
				return apis.NewApiError(500, "could not start transcode", nil)
			}
			go t.StartTranscode() //transcoding in separate thread, this confirms requested successfully
			return c.JSON(200, map[string]string{"message": "transcode requested"})
		})

		return nil //return no error on BeforeServe
	})
}

func getBroadcasters(workDir string) ([]*Broadcaster, error) {
	var bUrls []*Broadcaster
	//process seg list
	rf, err := os.Open(workDir + "/broadcasters.list")
	if err != nil {
		rf.Close()
		return nil, err
	}

	fs := bufio.NewReader(rf)
	curLine := 0
	for {
		line, _, err := fs.ReadLine()
		if err != nil {
			break
		}

		if len(line) > 0 {
			bUrlSplit := strings.Split(string(line), "|")
			u, bErr := url.ParseRequestURI(bUrlSplit[0])
			if bErr != nil {
				ErrorLogger.Printf("broadcaster list - could not parse url: %v", line)
				continue
			} else {

			}
			if len(bUrlSplit) > 2 {
				bUrls = append(bUrls, &Broadcaster{Url: u, User: bUrlSplit[1], Password: bUrlSplit[2]})
			}

		}

		curLine++
	}

	//all urls parsed
	return bUrls, nil
}

func initTus(app *pocketbase.PocketBase) (*tusd.Handler, error) {
	uploadPath := app.DataDir() + "/uploads"
	_, err := os.Stat(uploadPath)
	if err != nil {
		return nil, err
	}
	//setup tus upload things
	store := filestore.FileStore{
		Path: app.DataDir() + "/uploads",
	}
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	return tusd.NewHandler(tusd.Config{
		BasePath:              "/upload/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
	})
}

func getUserCookie(c echo.Context) *http.Cookie {
	cookie, err := c.Cookie("livepeer-transcode")
	if err == nil {
		return cookie
	} else {
		return nil
	}
}

func createUserCookie(app *pocketbase.PocketBase, user *models.Record) *http.Cookie {
	token, err := tokens.NewRecordAuthToken(app, user)
	if err != nil {
		return nil
	}
	cookie := new(http.Cookie)
	cookie.Name = "livepeer-transcode"
	cookie.Value = token
	cookie.Expires = time.Now().Add(24 * time.Hour)
	cookie.Secure = true
	cookie.HttpOnly = true
	cookie.SameSite = http.SameSiteStrictMode

	return cookie
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func verifySig(from, msg, sigHex string) bool {
	sig, err := hexutil.Decode(sigHex)
	if err != nil {
		err = fmt.Errorf("invalid sig ('%s'), %w", sigHex, err)
		InfoLogger.Printf("%v\n", err.Error())
		return false
	}

	msgHash := accounts.TextHash([]byte(msg))
	// ethereum "black magic" :(
	if sig[crypto.RecoveryIDOffset] == 27 || sig[crypto.RecoveryIDOffset] == 28 {
		sig[crypto.RecoveryIDOffset] -= 27
	}

	pk, err := crypto.SigToPub(msgHash, sig)
	if err != nil {
		err = fmt.Errorf("failed to recover public key from sig ('%s'), %w", sigHex, err)
		InfoLogger.Printf("%v\n", err.Error())
		return false
	}

	recoveredAddr := crypto.PubkeyToAddress(*pk)
	return strings.EqualFold(from, recoveredAddr.Hex())
}

func saveTusUpload(app core.App) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			user, _ := c.Get(apis.ContextAuthRecordKey).(*models.Record)
			metadata := strings.Split(c.Request().Header["Upload-Metadata"][0], ",")
			fn, fn_err := b64.StdEncoding.DecodeString(strings.Split(metadata[0], " ")[1])
			ft, ft_err := b64.StdEncoding.DecodeString(strings.Split(metadata[1], " ")[1])
			if fn_err != nil || ft_err != nil {
				return next(c)
			}

			c.Response().Before(func() {
				//get filename
				loc := c.Response().Header().Get("Location")
				locFn := path.Base(loc)
				//save upload
				collection, _ := app.Dao().FindCollectionByNameOrId("uploads")
				record := models.NewRecord(collection)
				record.Set("user", user.Id)
				record.Set("localfile", app.DataDir()+"/uploads/"+locFn)
				record.Set("filename", fn)
				record.Set("filetype", ft)
				if err := app.Dao().SaveRecord(record); err != nil {
					ErrorLogger.Printf("could not save upload file %v\n", err.Error())
					//return apis.NewApiError(500, "could not add user", nil)
				}
			})

			return next(c)

		}
	}
}

func loadAuthContextFromCookie(app core.App) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			//set as guest for default
			c.Set("isGuest", true)

			tokenCookie, err := c.Request().Cookie("livepeer-transcode")
			if err != nil || tokenCookie.Value == "" {
				return next(c) // no token cookie
			}

			token := tokenCookie.Value

			claims, _ := security.ParseUnverifiedJWT(token)
			tokenType := cast.ToString(claims["type"])

			switch tokenType {
			case tokens.TypeAdmin:
				admin, err := app.Dao().FindAdminByToken(
					token,
					app.Settings().AdminAuthToken.Secret,
				)
				if err == nil && admin != nil {
					// "authenticate" the admin
					c.Set(apis.ContextAdminKey, admin)
					c.Set("isGuest", false)
				}
			case tokens.TypeAuthRecord:
				record, err := app.Dao().FindAuthRecordByToken(
					token,
					app.Settings().RecordAuthToken.Secret,
				)
				if err == nil && record != nil {
					// "authenticate" the app user
					c.Set(apis.ContextAuthRecordKey, record)
					c.Set("isGuest", false)
				}
			}

			return next(c)
		}
	}
}
