package adapterio

import "testing"

// TestTheClosedCheckNamesTheDocument checks a make answer with a member that the schema
// does not define: the open check, which a reader uses, accepts it, and the closed
// check, which Encode uses, fails it with a text that has no empty member name.
func TestTheClosedCheckNamesTheDocument(t *testing.T) {
	b := []byte(`{"id": "i-1", "extra": 1}`)
	if err := check(makeAnswerFile, b, false); err != nil {
		t.Errorf("the open check: %v", err)
	}
	const want = "the document fails a rule of machines/v1 make-answer.schema.json"
	if err := check(makeAnswerFile, b, true); err == nil || err.Error() != want {
		t.Errorf("the closed check: %v, want %q", err, want)
	}
}
