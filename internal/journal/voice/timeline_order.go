package voice

// Match the client's partial-order rules. A date is not an invented midnight;
// only known dates and disjoint non-approximate time ranges establish edges.
func validTimelineOrder(events []TimelineEvent) bool {
	indices := map[string]int{}
	for i, e := range events {
		if _, exists := indices[e.ID]; exists {
			return false
		}
		indices[e.ID] = i
	}
	edges := make([]map[int]bool, len(events))
	for i := range edges {
		edges[i] = map[int]bool{}
	}
	for i, e := range events {
		if e.AfterEventID != nil {
			from, exists := indices[*e.AfterEventID]
			if !exists || from == i {
				return false
			}
			edges[from][i] = true
		}
		for j, other := range events {
			if i == j || e.Day == "" || other.Day == "" {
				continue
			}
			// Days are canonical ISO dates, validated by callers.
			if e.Day < other.Day {
				edges[i][j] = true
			}
			if e.Day == other.Day {
				_, high, left := timelineMinuteBounds(e)
				low, _, right := timelineMinuteBounds(other)
				if left && right && high < low {
					edges[i][j] = true
				}
			}
		}
	}
	incoming := make([]int, len(events))
	for _, targets := range edges {
		for target := range targets {
			incoming[target]++
		}
	}
	queue := []int{}
	for i, count := range incoming {
		if count == 0 {
			queue = append(queue, i)
		}
	}
	for cursor := 0; cursor < len(queue); cursor++ {
		for target := range edges[queue[cursor]] {
			incoming[target]--
			if incoming[target] == 0 {
				queue = append(queue, target)
			}
		}
	}
	return len(queue) == len(events)
}

func timelineMinuteBounds(e TimelineEvent) (int, int, bool) {
	if e.Approximate {
		return 0, 0, false
	}
	switch e.Precision {
	case "day":
		return 0, 1439, true
	case "minute":
		if e.Minute != nil {
			return *e.Minute, *e.Minute, true
		}
	case "period":
		switch e.Period {
		case "earlyMorning":
			return 0, 359, true
		case "morning":
			return 360, 719, true
		case "noon":
			return 720, 839, true
		case "afternoon":
			return 840, 1079, true
		case "evening":
			return 1080, 1259, true
		case "night":
			return 1260, 1439, true
		}
	}
	return 0, 0, false
}
