package agent

import (
	"context"
	"testing"
)

func TestSessionSelectionRollback(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	original := a.Settings()
	for _, choice := range [][3]string{{"", "m", "high"}, {original.Provider, "", "high"}, {"unknown", "m", "high"}, {original.Provider, "m", "invalid"}} {
		if err := a.SelectSession(context.Background(), choice[0], choice[1], choice[2]); err == nil {
			t.Fatalf("accepted invalid selection: %v", choice)
		}
		if got := a.Settings(); got.Provider != original.Provider || got.Model != original.Model || got.Effort != original.Effort {
			t.Fatal("failed validation changed selection")
		}
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`CREATE TRIGGER fail_selection BEFORE UPDATE ON sessions BEGIN SELECT RAISE(FAIL, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.SelectSession(context.Background(), original.Provider, "other", "high"); err == nil {
		t.Fatal("storage failure ignored")
	}
	if got := a.Settings(); got.Model != original.Model || got.Effort != original.Effort {
		t.Fatal("storage failure did not restore selection")
	}
}
