package assess

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ronny/brisk/risk"
)

// A review plan tells an agent reviewer where to look and what to check.
// Every word of it is brisk's catalog or the team's base .brisk.yaml; the
// model only scores how much each area matters, so it can drop or reorder
// areas but never write text the reviewer reads.

type reviewArea struct {
	name   string
	checks []string
}

// reviewCatalog is brisk's own review text, by area.
var reviewCatalog = map[string]reviewArea{
	"access_checks": {"Access checks", []string{
		"List who can reach this code after the change and confirm each caller is allowed.",
		"Find or add a test that a caller without access is denied.",
	}},
	"stored_data": {"Stored data and migrations", []string{
		"Check what the change does to existing rows and files, and that it can be rolled back.",
		"Confirm old app versions still work while the change deploys.",
	}},
	"data_loss": {"Data deletion", []string{
		"Confirm the deleted or dropped data is backed up or no longer read anywhere.",
	}},
	"secrets": {"Secrets", []string{
		"Confirm no real credential is committed; if one is, move it to the secret store and rotate it.",
	}},
	"transport_security": {"Transport security", []string{
		"Confirm certificate and TLS checks stay on outside tests.",
	}},
	"money": {"Money handling", []string{
		"Check amounts, rounding and currency, and that a retry cannot charge or refund twice.",
	}},
	"contracts": {"Interfaces other code relies on", []string{
		"List the callers and clients of the changed interface and confirm each still works, including older clients.",
	}},
	"build_deploy": {"Build and deploy", []string{
		"Check the pipeline still builds, runs the tests, and deploys to the intended target.",
		"Check new configuration and environment values exist where the change runs.",
	}},
	"dependencies": {"Dependencies", []string{
		"Confirm each new or upgraded package is the intended one and is needed.",
	}},
	"tests": {"Tests", []string{
		"Say why each removed, skipped or weakened test is no longer needed, or restore it.",
	}},
	"error_handling": {"Error handling", []string{
		"Say what the caller sees when this code fails, and confirm the failure is returned or logged.",
	}},
	"concurrency": {"Concurrency", []string{
		"Check shared state for races, and that locks and goroutines end on every path.",
	}},
	"hidden_code": {"Hidden or generated code", []string{
		"Read the hidden, nested or generated files as normal code and confirm they are meant to ship.",
	}},
	"risk_policy": {"Risk policy", []string{
		"Confirm the change to brisk's policy is intended and agreed by the team.",
	}},
	"weak_verification": {"Untested behaviour", []string{
		"Find the behaviour the change alters that no test covers, and add or run a test for it.",
	}},
	"verification": {"Verification", []string{
		"Run the checks that failed in the session and confirm they pass.",
	}},
	"project_rule": {"Project rule", []string{
		"Check the change against the project rule or question that flagged it, by its id in the assessment's provenance.",
	}},
}

// reviewAreaFor maps rule IDs, finding kinds (as lang:<kind>) and model
// questions to catalog areas. IDs not listed get no area.
var reviewAreaFor = map[string]string{
	"auth": "access_checks", "lang:auth_logic_changed": "access_checks", "weakens_auth": "access_checks",
	"migration": "stored_data", "lang:sql_changed": "stored_data",
	"destructive_sql": "data_loss", "destroys_data": "data_loss",
	"secret_added": "secrets", "secrets_config": "secrets",
	"tls_disabled": "transport_security",
	"payments":     "money",
	"public_api":   "contracts", "proto_renumbered": "contracts", "breaks_contract": "contracts",
	"lang:exported_signature_changed": "contracts", "lang:exported_symbol_removed": "contracts",
	"ci_deploy": "build_deploy", "breaks_deploy": "build_deploy",
	"dependencies":  "dependencies",
	"test_disabled": "tests", "lang:test_skipped": "tests", "lang:test_deleted": "tests", "lang:assertion_removed": "tests",
	"lang:error_swallowed":     "error_handling",
	"lang:concurrency_changed": "concurrency",
	"gitignore_hides_code":     "hidden_code", "nested_repo": "hidden_code", "generated_without_source": "hidden_code", "generated_hand_edited": "hidden_code",
	"brisk_config":                "risk_policy",
	"session:verification_failed": "verification",
	"uncertainty":                 "weak_verification",
	"follow_up_fix":               "weak_verification",
}

// maxReviewQuestions caps the areas Jev scores; the rest stay unscored.
const maxReviewQuestions = 8

// maxAreaEvidence caps the evidence listed per area.
const maxAreaEvidence = 5

// ReviewCheckConfig is a team's own review area for a project rule or
// question, from review_checks in .brisk.yaml.
type ReviewCheckConfig struct {
	Area   string
	Checks []string
}

// planCandidates lists the review areas the rule hits support, strongest
// first. Project rules (config:<id>) use the team's review_checks when set.
func (p *Prepared) planCandidates() []risk.ReviewArea {
	var out []risk.ReviewArea
	idx := map[string]int{}
	for _, h := range p.Hits {
		p.addArea(&out, idx, h.ID, h.Tier, h.Evidence)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Required && !out[j].Required })
	return out
}

// addArea adds or strengthens the area for one rule, finding or question.
func (p *Prepared) addArea(out *[]risk.ReviewArea, idx map[string]int, id string, tier risk.Tier, ev []risk.Evidence) {
	var a risk.ReviewArea
	project, isProject := strings.CutPrefix(id, "config:")
	if !isProject && slices.ContainsFunc(p.Policy.Questions, func(q CustomQuestion) bool { return q.ID == id }) {
		project, isProject = id, true
	}
	switch c, ok := p.Policy.ReviewChecks[project]; {
	case isProject && ok:
		a = risk.ReviewArea{ID: "config_" + project, Area: c.Area, Source: "config"}
		for i, t := range c.Checks {
			a.Checks = append(a.Checks, risk.ReviewCheck{ID: fmt.Sprintf("config_%s.%d", project, i+1), Text: t})
		}
	case isProject:
		a = catalogArea("project_rule")
	case reviewAreaFor[id] != "":
		a = catalogArea(reviewAreaFor[id])
	default:
		return
	}
	i, seen := idx[a.ID]
	if !seen {
		i = len(*out)
		idx[a.ID] = i
		a.Evidence = []risk.Evidence{}
		*out = append(*out, a)
	}
	r := &(*out)[i]
	r.Required = r.Required || tier == risk.High
	for _, e := range ev {
		// A whole-file entry adds nothing once the file has a line-level
		// one, and is replaced by the first line-level one.
		samePath := func(x risk.Evidence) bool { return x.Path == e.Path }
		wholeFile := func(x risk.Evidence) bool { return x.Path == e.Path && x.StartLine == 0 && x.Symbol == "" }
		switch {
		case slices.Contains(r.Evidence, e):
		case e.StartLine == 0 && e.Symbol == "" && slices.ContainsFunc(r.Evidence, samePath):
		case slices.ContainsFunc(r.Evidence, wholeFile):
			r.Evidence[slices.IndexFunc(r.Evidence, wholeFile)] = e
		case len(r.Evidence) < maxAreaEvidence:
			r.Evidence = append(r.Evidence, e)
		}
	}
}

func catalogArea(id string) risk.ReviewArea {
	c := reviewCatalog[id]
	a := risk.ReviewArea{ID: id, Area: c.name, Source: "brisk"}
	for i, t := range c.checks {
		a.Checks = append(a.Checks, risk.ReviewCheck{ID: fmt.Sprintf("%s.%d", id, i+1), Text: t})
	}
	return a
}

// reviewQuestions asks Jev how much each candidate area matters.
func reviewQuestions(areas []risk.ReviewArea) map[string]risk.Question {
	if len(areas) == 0 {
		return nil
	}
	qs := map[string]risk.Question{}
	for _, a := range areas[:min(len(areas), maxReviewQuestions)] {
		qs["review_"+a.ID] = risk.Question{
			Type:         "score",
			Instructions: fmt.Sprintf("How much does a reviewer of this change need to check %q, that is: %s", a.Area, strings.Join(checkTexts(a), " ")) + guard,
			Criteria: []string{
				"Not at all: the change does not touch this.",
				"A little: the change touches it only incidentally.",
				"Clearly: the change affects it and a reviewer should check it.",
				"Most of all: this is the riskiest part of the change.",
			},
		}
	}
	return qs
}

func checkTexts(a risk.ReviewArea) []string {
	var out []string
	for _, c := range a.Checks {
		out = append(out, c.Text)
	}
	return out
}

// buildPlan returns the review plan and its provenance. fired are the
// model overrides that raised the tier; they add areas Jev did not score.
// review holds Jev's answers to the review questions, or nil.
//
// answers are the main answers, or nil: an answer that meets a
// human_check condition adds its area too, so a person asked for by an
// answer alone has something to consider.
func (p *Prepared) buildPlan(fired []string, firedTier map[string]risk.Tier, answers, review map[string]risk.Answer) ([]risk.ReviewArea, *risk.ReviewPlanProvenance) {
	areas := p.planCandidates()
	idx := map[string]int{}
	for i, a := range areas {
		idx[a.ID] = i
	}
	asked := len(p.ReviewQuestions)
	for _, q := range fired {
		p.addArea(&areas, idx, q, firedTier[q], nil)
	}
	for _, q := range p.Policy.ReviewTriggers(answers) {
		p.addArea(&areas, idx, q, risk.Low, nil)
	}
	prov := &risk.ReviewPlanProvenance{Scores: map[string]float64{}, Dropped: []string{}}
	if asked > 0 && review != nil {
		prov.Tailored, prov.Status = true, "tailored"
		for q := range p.ReviewQuestions {
			if a, ok := review[q]; !ok || a.Score == nil {
				prov.Tailored, prov.Status = false, ""
			}
		}
	}
	if !prov.Tailored {
		if len(areas) == 0 {
			return []risk.ReviewArea{}, nil
		}
		// Override areas were appended; keep required ones first.
		sort.SliceStable(areas, func(i, j int) bool { return areas[i].Required && !areas[j].Required })
		return areas, prov
	}
	kept := areas[:0:0]
	for _, a := range areas {
		// Only answers to questions brisk asked count.
		if _, asked := p.ReviewQuestions["review_"+a.ID]; !asked {
			kept = append(kept, a)
			continue
		}
		if ans, ok := review["review_"+a.ID]; ok {
			s := round2(*ans.Score)
			a.Score = &s
			prov.Scores[a.ID] = s
			if s < 1 && !a.Required {
				prov.Dropped = append(prov.Dropped, a.ID)
				continue
			}
		}
		kept = append(kept, a)
	}
	// Required areas first, led by unscored ones from High overrides; then
	// areas Jev scored, by score; then the unscored ones (beyond the cap,
	// or added from the answers), which are never dropped but must not
	// outrank what Jev judged relevant.
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		switch {
		case a.Required != b.Required:
			return a.Required
		case a.Required && (a.Score == nil) != (b.Score == nil):
			// A required area left unscored came from a High override,
			// which outranks any score.
			return a.Score == nil
		case (a.Score == nil) != (b.Score == nil):
			return a.Score != nil
		case a.Score != nil:
			return *a.Score > *b.Score
		}
		return false
	})
	return kept, prov
}

// humanPrompts is brisk's question for a person, by review area: what to
// think about rather than what to check line by line, which the agent
// review does.
var humanPrompts = map[string]string{
	"access_checks":      "Who should be allowed to do this, and does the change still enforce it?",
	"stored_data":        "What happens to existing data while this deploys, and how would you roll it back?",
	"data_loss":          "Is it safe to lose the data this deletes, and is there a way back if not?",
	"secrets":            "Is this a real credential, and if so, who rotates it?",
	"transport_security": "Should certificate checks really be off anywhere outside tests?",
	"money":              "Could this charge, refund or round anyone's money wrongly, including on a retry?",
	"contracts":          "Who depends on this interface, and will they break when it ships?",
	"build_deploy":       "Will this build, deploy and start where it runs, with the configuration it needs?",
	"dependencies":       "Do you want this new or upgraded dependency in the project?",
	"tests":              "Is it right that these tests are removed, skipped or weakened?",
	"error_handling":     "When this fails, will someone notice, and what will users see?",
	"concurrency":        "Could two requests or workers race here, and what would break if they did?",
	"hidden_code":        "Should these hidden, nested or generated files ship as they are?",
	"risk_policy":        "Does the team agree with this change to brisk's risk policy?",
	"weak_verification":  "Which behaviour does this change that nothing has tested, and is that acceptable?",
	"verification":       "Checks failed during the session; is it right to ship before they pass?",
	"project_rule":       "Does the change still respect the project rule or question that flagged it?",
	"change_scope":       "Is this the change you meant to make, and is now the right time to ship it?",
}

// maxConsider caps human_check.consider.
const maxConsider = 2

// consider picks what a person should think about: the top review areas,
// in plan order, each as one question. A team's own area (source config)
// asks its first check. Nothing when no person is needed.
func consider(needed bool, plan []risk.ReviewArea) []risk.Consider {
	out := []risk.Consider{}
	if !needed {
		return out
	}
	for _, a := range plan {
		if len(out) == maxConsider {
			break
		}
		c := risk.Consider{ID: a.ID, Source: a.Source, Text: humanPrompts[a.ID], Evidence: append([]risk.Evidence{}, a.Evidence[:min(len(a.Evidence), 3)]...)}
		if a.Source == "config" && len(a.Checks) > 0 {
			c.Text = a.Checks[0].Text
		}
		if c.Text == "" {
			continue
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		out = append(out, risk.Consider{ID: "change_scope", Source: "brisk", Text: humanPrompts["change_scope"], Evidence: []risk.Evidence{}})
	}
	return out
}
