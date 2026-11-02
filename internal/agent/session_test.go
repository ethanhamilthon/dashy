package agent

import (
	"os"
	"testing"
)

func TestSaveFailurePreservesSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.message("user", "keep")

	if err = os.Mkdir(a.HistoryPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.NewSession(); err == nil {
		t.Fatal("expected save error")
	}
	if len(a.History) != 1 || len(a.Records()) != 1 {
		t.Fatal("save failure discarded history")
	}
}
