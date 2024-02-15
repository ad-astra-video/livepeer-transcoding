/// <reference path="../pb_data/types.d.ts" />
migrate((db) => {
  const dao = new Dao(db)
  const collection = dao.findCollectionByNameOrId("1oe3eocshms1c81")

  collection.listRule = "@request.auth.id != \"\" && user = @request.auth.id"
  collection.viewRule = "@request.auth.id != \"\" && user = @request.auth.id"
  collection.createRule = "@request.auth.id != \"\" && user = @request.auth.id"
  collection.updateRule = "@request.auth.id != \"\" && user = @request.auth.id"
  collection.deleteRule = "@request.auth.id != \"\" && user = @request.auth.id"

  // add
  collection.schema.addField(new SchemaField({
    "system": false,
    "id": "3hmxoumw",
    "name": "status",
    "type": "select",
    "required": false,
    "presentable": false,
    "unique": false,
    "options": {
      "maxSelect": 1,
      "values": [
        "in_progress",
        "complete",
        "error"
      ]
    }
  }))

  // add
  collection.schema.addField(new SchemaField({
    "system": false,
    "id": "y2zg4gvu",
    "name": "status_message",
    "type": "text",
    "required": false,
    "presentable": false,
    "unique": false,
    "options": {
      "min": null,
      "max": null,
      "pattern": ""
    }
  }))

  // update
  collection.schema.addField(new SchemaField({
    "system": false,
    "id": "xlojdfpl",
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
  }))

  return dao.saveCollection(collection)
}, (db) => {
  const dao = new Dao(db)
  const collection = dao.findCollectionByNameOrId("1oe3eocshms1c81")

  collection.listRule = null
  collection.viewRule = null
  collection.createRule = null
  collection.updateRule = null
  collection.deleteRule = null

  // remove
  collection.schema.removeField("3hmxoumw")

  // remove
  collection.schema.removeField("y2zg4gvu")

  // update
  collection.schema.addField(new SchemaField({
    "system": false,
    "id": "xlojdfpl",
    "name": "field",
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
  }))

  return dao.saveCollection(collection)
})
