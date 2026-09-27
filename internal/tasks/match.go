package tasks

import "github.com/bmatcuk/doublestar/v4"

// Matches reports whether in fires one of t's triggers and t's if holds
// for it. A normalized trigger matches in.Event; a raw one matches
// in.RawEvent whatever in.Event is. Whether the sender is kritik's own
// bot is the caller's to check.
func (t *Task) Matches(in Input) (bool, error) {
	fired := false
	for _, tr := range t.On {
		if tr.matches(in) {
			fired = true
			break
		}
	}
	if !fired {
		return false, nil
	}
	g, err := compileGuard(t.If, false)
	if err != nil {
		return false, err
	}
	return g.eval(vars(in, nil))
}

func (tr Trigger) matches(in Input) bool {
	if tr.Event == EventRaw {
		if ok, _ := doublestar.Match(tr.RawEvent, in.RawEvent); !ok || in.RawEvent == "" {
			return false
		}
	} else if in.Event == "" || tr.Event != in.Event {
		return false
	}
	return len(tr.Actions) == 0 || matchAny(tr.Actions, in.Action)
}

// Names are the event names the trigger fires on, as the operator's
// bounds glob them: "issue.opened", or "raw:release.published" for a raw
// trigger, with "*" for any action.
func (tr Trigger) Names() []string {
	prefix := tr.Event + "."
	if tr.Event == EventRaw {
		prefix = "raw:" + tr.RawEvent + "."
	}
	if len(tr.Actions) == 0 {
		return []string{prefix + "*"}
	}
	out := make([]string, len(tr.Actions))
	for i, a := range tr.Actions {
		out[i] = prefix + a
	}
	return out
}
