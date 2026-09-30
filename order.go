package automation

import "sort"

func sortRules(rules []Rule) {
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].ID < rules[j].ID
	})
}

func sortActions(actions []Action) {
	sort.Slice(actions, func(i, j int) bool {
		if !actions[i].CompleteAt.Equal(actions[j].CompleteAt) {
			return actions[i].CompleteAt.Before(actions[j].CompleteAt)
		}
		return actions[i].ID < actions[j].ID
	})
}

// sortEvents 按时间稳定排序，保证历史重放结果与实时处理一致。
func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Time.Equal(events[j].Time) {
			return events[i].Time.Before(events[j].Time)
		}
		return false
	})
}
