package assess

import "testing"

// Every catalog area has a question for a person, so no area a plan can
// hold is skipped in human_check.consider.
func TestEveryAreaHasAHumanPrompt(t *testing.T) {
	for id := range reviewCatalog {
		if humanPrompts[id] == "" {
			t.Errorf("review area %s has no human prompt", id)
		}
	}
}
