package control

import (
	"fmt"
	"sync"
	"testing"

	"dashy/internal/agent"
)

func TestConcurrentInputDuringControllerInitialization(t *testing.T) {
	for round := 0; round < 20; round++ {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			var producers sync.WaitGroup
			h := newHarnessWith(t, &fakeModel{}, func(a *agent.Agent) {
				for i := 0; i < 4; i++ {
					producers.Add(1)
					go func(i int) {
						defer producers.Done()
						for j := 0; j < 8; j++ {
							a.Queue().PushNotice(agent.Input{Text: fmt.Sprintf("notice-%d-%d", i, j), Notice: true})
						}
					}(i)
				}
			})
			producers.Wait()
			h.await("queued turn", func(events []Event) bool { return len(doneEvents(events)) > 0 })
			h.ctl.Close()
			if len(h.model.requests()) == 0 {
				t.Fatal("queued input lost")
			}
		})
	}
}
