package scores

import (
	"encoding/json"
	"testing"
)

// TestFileRoundTrip pins the on-disk json tags and the empty-map
// marshaling contract: an empty (non-nil) Assignments map must serialize
// as `{}`, not `null`, so collect_scores.py sees a well-formed file on
// the first run after scaffold.
func TestFileRoundTrip(t *testing.T) {
	f := File{Schema: SchemaV1, Assignments: map[string]AssignmentBucket{}}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	want := `{"schema":"classroom50/scores/v1","assignments":{}}`
	if got != want {
		t.Errorf("marshal = %s, want %s", got, want)
	}

	var back File
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Schema != SchemaV1 {
		t.Errorf("schema round-trip = %q, want %q", back.Schema, SchemaV1)
	}
	if back.Assignments == nil {
		t.Error("assignments must decode as non-nil empty map")
	}
}

// TestAssignmentBucketDecodesTolerantEntries confirms each bucket entry
// stays a tolerant map[string]any (download reads only well-known keys),
// and the type/entries tags decode as expected.
func TestAssignmentBucketDecodesTolerantEntries(t *testing.T) {
	in := `{"schema":"classroom50/scores/v1","assignments":{"hw1":{"type":"individual","entries":[{"owner":"alice","extra":42}]}}}`
	var f File
	if err := json.Unmarshal([]byte(in), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	bucket, ok := f.Assignments["hw1"]
	if !ok {
		t.Fatal("expected hw1 bucket")
	}
	if bucket.Type != "individual" {
		t.Errorf("type = %q, want individual", bucket.Type)
	}
	if len(bucket.Entries) != 1 || bucket.Entries[0]["owner"] != "alice" {
		t.Errorf("entries = %v, want one entry owned by alice", bucket.Entries)
	}
	if bucket.Entries[0]["extra"] != float64(42) {
		t.Errorf("tolerant entry should retain unknown key 'extra', got %v", bucket.Entries[0]["extra"])
	}
}
