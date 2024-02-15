/// <reference path="../pb_data/types.d.ts" />
migrate((db) => {
  const collection = new Collection({
    "id": "hg75p2k9q083hdp",
    "created": "2023-11-12 04:48:14.036Z",
    "updated": "2023-11-12 04:48:14.036Z",
    "name": "settings",
    "type": "base",
    "system": false,
    "schema": [
      {
        "system": false,
        "id": "ncg79x1f",
        "name": "settings",
        "type": "json",
        "required": false,
        "presentable": false,
        "unique": false,
        "options": {}
      },
      {
        "system": false,
        "id": "6ttqjv5d",
        "name": "user",
        "type": "relation",
        "required": false,
        "presentable": false,
        "unique": false,
        "options": {
          "collectionId": "_pb_users_auth_",
          "cascadeDelete": false,
          "minSelect": null,
          "maxSelect": 1,
          "displayFields": null
        }
      }
    ],
    "indexes": [],
    "listRule": "@request.auth.id != \"\" && user = @request.auth.id",
    "viewRule": "@request.auth.id != \"\" && user = @request.auth.id",
    "createRule": "@request.auth.id != \"\" && user = @request.auth.id",
    "updateRule": "@request.auth.id != \"\" && user = @request.auth.id",
    "deleteRule": "@request.auth.id != \"\" && user = @request.auth.id",
    "options": {}
  });

  return Dao(db).saveCollection(collection);
}, (db) => {
  const dao = new Dao(db);
  const collection = dao.findCollectionByNameOrId("hg75p2k9q083hdp");

  return dao.deleteCollection(collection);
})
