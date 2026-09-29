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
