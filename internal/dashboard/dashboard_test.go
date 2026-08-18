package dashboard

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestShouldStart(t *testing.T) {
	tests := []struct {
		workerID string
		want     bool
	}{{"spritesheet_host@1", true}, {"spritesheet_host@2", false}, {"spritesheet_manual", true}}
	for _, test := range tests {
		if got := ShouldStart(test.workerID); got != test.want {
			t.Errorf("ShouldStart(%q) = %v, want %v", test.workerID, got, test.want)
		}
	}
}

func TestTimelineSteps(t *testing.T) {
	timeline := bson.D{{Key: "media", Value: bson.D{{Key: "status", Value: "pending"}}}, {Key: "generate", Value: bson.D{{Key: "status", Value: "processing"}, {Key: "percent", Value: int32(55)}}}, {Key: "prepare", Value: bson.D{{Key: "status", Value: "completed"}, {Key: "percent", Value: int32(90)}}}}
	steps := timelineSteps(timeline)
	if len(steps) != 3 {
		t.Fatalf("got %d steps", len(steps))
	}
	if steps[0].Key != "prepare" || steps[0].Percent != 100 {
		t.Fatalf("unexpected first step: %#v", steps[0])
	}
	if steps[1].Key != "generate" || steps[1].Percent != 55 {
		t.Fatalf("unexpected generate step: %#v", steps[1])
	}
}

func TestParseCodecUtilization(t *testing.T) {
	got := parseCodecUtilization("# gpu sm mem enc dec\n0 12 4 7 63\n")
	if got[0].encoder != 7 || got[0].decoder != 63 {
		t.Fatalf("unexpected utilization: %#v", got[0])
	}
}
