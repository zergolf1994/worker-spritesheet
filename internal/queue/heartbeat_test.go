package queue

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestHeartbeatPreservesAdminEnable(t *testing.T) {
	setFields := bson.M{}
	setOnInsert := bson.M{"enable": true}
	applyHeartbeatEnable(setFields, setOnInsert, false)
	if _, exists := setFields["enable"]; exists {
		t.Fatal("normal heartbeat must not overwrite admin enable")
	}
	if setOnInsert["enable"] != true {
		t.Fatal("new worker should default to enabled")
	}
}

func TestHeartbeatDiskPauseOnlyDisables(t *testing.T) {
	setFields := bson.M{}
	setOnInsert := bson.M{"enable": true}
	applyHeartbeatEnable(setFields, setOnInsert, true)
	if setFields["enable"] != false {
		t.Fatal("disk safety pause should disable the worker")
	}
	if _, exists := setOnInsert["enable"]; exists {
		t.Fatal("disk-paused insert must not conflict with enable=false")
	}
}
