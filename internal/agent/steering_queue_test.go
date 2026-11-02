package agent

import (
	"reflect"
	"sync"
	"testing"
)

func TestSteeringRecallOrderingAndAcceptanceRace(t *testing.T) {
	s := &Steering{}
	s.Push("first")
	s.Push("second")
	if text, ok := s.RecallInput(); !ok || text.Text != "second" {
		t.Fatal("recall must return newest pending instruction")
	}
	s.Push("edited second")
	if got := s.TakeInputs(); !reflect.DeepEqual(got, []Input{{Text: "first"}, {Text: "edited second"}}) {
		t.Fatal(got)
	}
	if _, ok := s.RecallInput(); ok {
		t.Fatal("accepted instruction recalled")
	}
	for n := 0; n < 100; n++ {
		queue := &Steering{}
		queue.Push("unique")
		var taken []Input
		var recalled bool
		var group sync.WaitGroup
		group.Add(2)
		go func() { defer group.Done(); taken = queue.TakeInputs() }()
		go func() { defer group.Done(); _, recalled = queue.RecallInput() }()
		group.Wait()
		if (len(taken) == 1) == recalled {
			t.Fatal("instruction was lost or both recalled and accepted")
		}
	}
}
