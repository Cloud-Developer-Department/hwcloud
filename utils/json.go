package utils

import "encoding/json"

// PrettyJSON re-indents a JSON string for display. If the input is not
// valid JSON it is returned as-is.
func PrettyJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return s
	}
	return string(b)
}
