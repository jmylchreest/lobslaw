package console

import (
	"encoding/json"
	"strconv"
)

// revisionJSON keeps existing small-number REST clients compatible, while
// revisions beyond JavaScript's exact integer range travel as decimal strings.
type revisionJSON uint64

func (v revisionJSON) MarshalJSON() ([]byte, error) {
	text := strconv.FormatUint(uint64(v), 10)
	if uint64(v) > 9007199254740991 {
		return json.Marshal(text)
	}
	return []byte(text), nil
}
func (v *revisionJSON) UnmarshalJSON(raw []byte) error {
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return err
		}
	} else {
		text = string(raw)
	}
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return err
	}
	*v = revisionJSON(n)
	return nil
}

func (v BotView) MarshalJSON() ([]byte, error) {
	type plain BotView
	return json.Marshal(struct {
		*plain
		Revision revisionJSON `json:"revision"`
	}{(*plain)(&v), revisionJSON(v.Revision)})
}
func (v *BotView) UnmarshalJSON(raw []byte) error {
	type plain BotView
	var revision revisionJSON
	body := struct {
		*plain
		Revision *revisionJSON `json:"revision"`
	}{(*plain)(v), &revision}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	v.Revision = uint64(revision)
	return nil
}

func (v GroupView) MarshalJSON() ([]byte, error) {
	type plain GroupView
	return json.Marshal(struct {
		*plain
		Revision revisionJSON `json:"revision"`
	}{(*plain)(&v), revisionJSON(v.Revision)})
}
func (v *GroupView) UnmarshalJSON(raw []byte) error {
	type plain GroupView
	var revision revisionJSON
	body := struct {
		*plain
		Revision *revisionJSON `json:"revision"`
	}{(*plain)(v), &revision}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	v.Revision = uint64(revision)
	return nil
}

func (v InboxItemView) MarshalJSON() ([]byte, error) {
	type plain InboxItemView
	return json.Marshal(struct {
		*plain
		Revision revisionJSON `json:"revision"`
	}{(*plain)(&v), revisionJSON(v.Revision)})
}
func (v *InboxItemView) UnmarshalJSON(raw []byte) error {
	type plain InboxItemView
	var revision revisionJSON
	body := struct {
		*plain
		Revision *revisionJSON `json:"revision"`
	}{(*plain)(v), &revision}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	v.Revision = uint64(revision)
	return nil
}

func (v *BotPatch) UnmarshalJSON(raw []byte) error {
	type plain BotPatch
	body := struct {
		*plain
		Revision *revisionJSON `json:"revision"`
	}{plain: (*plain)(v)}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	v.Revision = nil
	if body.Revision != nil {
		revision := uint64(*body.Revision)
		v.Revision = &revision
	}
	return nil
}

func (v *GroupInput) UnmarshalJSON(raw []byte) error {
	type plain GroupInput
	body := struct {
		*plain
		Revision *revisionJSON `json:"revision"`
	}{plain: (*plain)(v)}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	v.Revision = nil
	if body.Revision != nil {
		revision := uint64(*body.Revision)
		v.Revision = &revision
	}
	return nil
}
