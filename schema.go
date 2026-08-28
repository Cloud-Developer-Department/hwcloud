package hwcloud

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Parameters is the provider-neutral tool-parameter model: a structured
// JSON Schema subset (object with properties). It is generated from Go
// structs via SchemaOf, so the schema and the argument struct can never
// drift apart, and serialized per provider (OpenAI "parameters",
// Anthropic "input_schema", MCP "inputSchema") by the model adapters.
type Parameters struct {
	Type       string                `json:"type"`
	Properties map[string]*Parameter `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

// Parameter describes one tool argument (or a nested object/array item).
type Parameter struct {
	Type        string                `json:"type"`
	Description string                `json:"description,omitempty"`
	Enum        []string              `json:"enum,omitempty"`
	Items       *Parameter            `json:"items,omitempty"`      // array element type
	Properties  map[string]*Parameter `json:"properties,omitempty"` // nested object
	Required    []string              `json:"required,omitempty"`   // nested object required
}

// SchemaOf generates the Parameters model from a Go struct:
//
//   - field name: json tag (snake_case fallback)
//
//   - required:   fields WITHOUT ",omitempty" in the json tag are required
//     (Go convention: omitempty = optional)
//
//   - description/enum/required override: jsonschema tag
//     (jsonschema:"description=...,enum=a,b,required" — compatible with
//     invopop/jsonschema so the generator can be swapped for the library)
//
//   - supported types: string, integer (int/int64/...), number
//     (float32/64), boolean, []T, map/any, nested structs (recursive)
//
//     type ReadFileParams struct {
//     Path string `json:"path" jsonschema:"description=Absolute path to read"`
//     Line int    `json:"line" jsonschema:"description=1-based start line"`
//     }
//     Parameters: SchemaOf[ReadFileParams]()
func SchemaOf[T any]() *Parameters {
	var zero T
	return buildParameters(reflect.TypeOf(zero))
}

// ParseArgs unmarshals tool arguments into T and validates required
// parameters (from the same struct tags SchemaOf reads). Unknown fields
// are ignored.
//
// Empty arguments are treated as an empty object: required-parameter
// validation still runs. (Skipping it meant a model emitting a tool_call
// with no arguments silently got a zero-value struct — e.g. grep with an
// empty pattern matching the entire workspace.)
func ParseArgs[T any](args json.RawMessage) (T, error) {
	var p T
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return p, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if err := validateRequired(reflect.TypeOf(p), reflect.ValueOf(p)); err != nil {
		return p, err
	}
	return p, nil
}

// buildParameters reflects a type into the Parameters model.
func buildParameters(typ reflect.Type) *Parameters {
	typ = deref(typ)
	p := &Parameters{Type: "object", Properties: map[string]*Parameter{}}
	if typ.Kind() != reflect.Struct {
		return p
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name := fieldName(f)
		if name == "-" {
			continue
		}
		param := buildParameter(f.Type)
		if d := f.Tag.Get("jsonschema"); d != "" {
			applyJSONSchemaTag(param, d, &name)
		}
		p.Properties[name] = param
		if isRequired(f) {
			p.Required = append(p.Required, name)
		}
	}
	return p
}

// buildParameter reflects a field type into a Parameter.
func buildParameter(t reflect.Type) *Parameter {
	t = deref(t)
	p := &Parameter{Type: typeName(t)}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		p.Type = "array"
		p.Items = buildParameter(t.Elem())
	case reflect.Struct:
		p.Properties = map[string]*Parameter{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := fieldName(f)
			if name == "-" {
				continue
			}
			child := buildParameter(f.Type)
			if d := f.Tag.Get("jsonschema"); d != "" {
				applyJSONSchemaTag(child, d, nil)
			}
			p.Properties[name] = child
			if isRequired(f) {
				p.Required = append(p.Required, name)
			}
		}
	}
	return p
}

// applyJSONSchemaTag parses jsonschema:"description=...,enum=a,b,required"
// into the parameter. It may also set the field name (for top-level
// fields where the tag carries a name= override — rare, but supported).
func applyJSONSchemaTag(p *Parameter, tag string, name *string) {
	for _, kv := range strings.Split(tag, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		key, value, hasValue := strings.Cut(kv, "=")
		switch key {
		case "description":
			if hasValue {
				p.Description = value
			}
		case "enum":
			if hasValue {
				p.Enum = strings.Split(value, ",")
			}
		case "required":
			// explicit required marker; used with omitempty-optional fields
			// (handled at the struct level, not here — see callers)
		case "name":
			if hasValue && name != nil {
				*name = value
			}
		}
	}
}

// isRequired reports whether a field is a required parameter: no
// ",omitempty" in the json tag, or an explicit jsonschema:"required"
// marker.
func isRequired(f reflect.StructField) bool {
	jt := f.Tag.Get("json")
	if jt != "" && strings.Contains(jt, ",omitempty") {
		// explicit "required" in jsonschema tag overrides omitempty
		return strings.Contains(f.Tag.Get("jsonschema"), "required")
	}
	return true
}

// typeName maps a Go type to its JSON Schema type name.
func typeName(t reflect.Type) string {
	t = deref(t)
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Interface:
		// No per-key info at schema time: degrade to a loose object
		// (maps are objects in JSON; "array" was wrong).
		return "object"
	default:
		return "object"
	}
}

// fieldName resolves the JSON field name from the json tag.
func fieldName(f reflect.StructField) string {
	jt := f.Tag.Get("json")
	if jt == "" {
		return toSnake(f.Name)
	}
	return strings.Split(jt, ",")[0]
}

// toSnake converts CamelCase to snake_case for tag-less fields.
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// deref unwraps pointer types.
func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// validateRequired checks required fields after unmarshal. Required
// strings must be non-empty, slices/maps non-nil, pointers non-nil.
// Numeric and boolean required fields pass (0/false are valid inputs).
func validateRequired(typ reflect.Type, val reflect.Value) error {
	if typ.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil
		}
		typ, val = typ.Elem(), val.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() || !isRequired(f) {
			continue
		}
		name := fieldName(f)
		if name == "-" {
			continue
		}
		v := val.Field(i)
		switch v.Kind() {
		case reflect.String:
			if v.String() == "" {
				return fmt.Errorf("missing required parameter %q", name)
			}
		case reflect.Slice, reflect.Map:
			if v.IsNil() {
				return fmt.Errorf("missing required parameter %q", name)
			}
		case reflect.Ptr:
			if v.IsNil() {
				return fmt.Errorf("missing required parameter %q", name)
			}
		}
	}
	return nil
}

// SchemaMap converts the neutral Parameters model back to a plain JSON
// Schema map (the form model providers and MCP expect on the wire). Since
// the model's JSON form is a JSON Schema, this is a marshal/unmarshal
// round-trip.
func (p *Parameters) SchemaMap() map[string]any {
	if p == nil {
		return map[string]any{"type": "object"}
	}
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ParametersFromMap builds the neutral Parameters model from a JSON
// Schema map (e.g. an MCP tool's InputSchema or an OpenAI-style schema
// received from an adapter). Unknown keys are ignored; missing "type"
// defaults to "object".
func ParametersFromMap(m map[string]any) *Parameters {
	p := &Parameters{Type: "object", Properties: map[string]*Parameter{}}
	if m == nil {
		return p
	}
	if t, ok := m["type"].(string); ok {
		p.Type = t
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				p.Required = append(p.Required, s)
			}
		}
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, v := range props {
			if pm, ok := v.(map[string]any); ok {
				p.Properties[name] = parameterFromMap(pm)
			}
		}
	}
	return p
}

// parameterFromMap builds a Parameter from a JSON Schema property map.
func parameterFromMap(m map[string]any) *Parameter {
	p := &Parameter{Type: "string"}
	if t, ok := m["type"].(string); ok {
		p.Type = t
	}
	if d, ok := m["description"].(string); ok {
		p.Description = d
	}
	if e, ok := m["enum"].([]any); ok {
		for _, v := range e {
			if s, ok := v.(string); ok {
				p.Enum = append(p.Enum, s)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		p.Items = parameterFromMap(items)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		p.Properties = map[string]*Parameter{}
		for name, v := range props {
			if pm, ok := v.(map[string]any); ok {
				p.Properties[name] = parameterFromMap(pm)
			}
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				p.Required = append(p.Required, s)
			}
		}
	}
	return p
}
