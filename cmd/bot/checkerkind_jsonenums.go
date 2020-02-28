// generated by jsonenums -type=checkerKind; DO NOT EDIT

package main

import (
	"encoding/json"
	"fmt"
)

var (
	_checkerKindNameToValue = map[string]checkerKind{
		"checkerAPI":     checkerAPI,
		"checkerPolling": checkerPolling,
	}

	_checkerKindValueToName = map[checkerKind]string{
		checkerAPI:     "checkerAPI",
		checkerPolling: "checkerPolling",
	}
)

func init() {
	var v checkerKind
	if _, ok := interface{}(v).(fmt.Stringer); ok {
		_checkerKindNameToValue = map[string]checkerKind{
			interface{}(checkerAPI).(fmt.Stringer).String():     checkerAPI,
			interface{}(checkerPolling).(fmt.Stringer).String(): checkerPolling,
		}
	}
}

// MarshalJSON is generated so checkerKind satisfies json.Marshaler.
func (r checkerKind) MarshalJSON() ([]byte, error) {
	if s, ok := interface{}(r).(fmt.Stringer); ok {
		return json.Marshal(s.String())
	}
	s, ok := _checkerKindValueToName[r]
	if !ok {
		return nil, fmt.Errorf("invalid checkerKind: %d", r)
	}
	return json.Marshal(s)
}

// UnmarshalJSON is generated so checkerKind satisfies json.Unmarshaler.
func (r *checkerKind) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("checkerKind should be a string, got %s", data)
	}
	v, ok := _checkerKindNameToValue[s]
	if !ok {
		return fmt.Errorf("invalid checkerKind %q", s)
	}
	*r = v
	return nil
}
