package agent

// answerMetadata derives turn totals without counting a request once per item.
// Legacy answers already contain turn totals and retain their original metadata.
func (a *Agent) answerMetadata(seq int) Metadata {
	m := *a.itemMeta[seq]
	if m.RequestSeq == 0 {
		return m
	}
	m.Duration, m.Usage = 0, nil
	var total Usage
	known := true
	for i := m.TurnSeq; i <= seq; i++ {
		part := a.itemMeta[i]
		if part == nil || (part.RequestSeq != 0 && part.RequestSeq != i+1) {
			continue
		}
		m.Duration += part.Duration
		if part.RequestSeq == 0 {
			continue
		}
		if part.Usage == nil {
			known = false
			continue
		}
		total.Input += part.Usage.Input
		total.Output += part.Usage.Output
		if total.Cached < 0 || part.Usage.Cached < 0 {
			total.Cached = -1
		} else {
			total.Cached += part.Usage.Cached
		}
	}
	if known {
		m.Usage = &total
	}
	return m
}
