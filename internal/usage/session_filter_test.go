package usage

import "testing"

func TestFilterSessionIsExactAndAcceptsNativeSession(t *testing.T) {
	f := Filter{Session: "session-a"}
	if !f.keeps(Record{Session: "session-a"}) {
		t.Fatal("exact session was rejected")
	}
	if !f.keeps(Record{Session: "gateway-alias", NativeSession: "session-a"}) {
		t.Fatal("native session was rejected")
	}
	if f.keeps(Record{Session: "session-a-extra"}) {
		t.Fatal("substring session was accepted")
	}
}
