package exprhelpers

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJsonExtract(t *testing.T) {
	if err := Init(nil); err != nil {
		t.Fatal(err)
	}

	err := FileInit("testdata", "test_data_re.txt", "regex")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		jsonBlob     string
		targetField  string
		expectResult string
		expr         string
	}{
		{
			name:         "basic json extract",
			jsonBlob:     `{"test" : "1234"}`,
			targetField:  "test",
			expectResult: "1234",
			expr:         "JsonExtract(blob, target)",
		},
		{
			name:         "basic json extract with non existing field",
			jsonBlob:     `{"test" : "1234"}`,
			targetField:  "non_existing_field",
			expectResult: "",
			expr:         "JsonExtract(blob, target)",
		},
		{
			name:         "extract subfield",
			jsonBlob:     `{"test" : {"a": "b"}}`,
			targetField:  "test.a",
			expectResult: "b",
			expr:         "JsonExtract(blob, target)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]interface{}{
				"blob":   test.jsonBlob,
				"target": test.targetField,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			out, err := expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, out)
		})
	}
}

func TestJsonExtractUnescape(t *testing.T) {
	if err := Init(nil); err != nil {
		t.Fatal(err)
	}

	err := FileInit("testdata", "test_data_re.txt", "regex")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		jsonBlob     string
		targetField  string
		expectResult string
		expr         string
	}{
		{
			name:         "basic json extract",
			jsonBlob:     `{"log" : "\"GET /JBNwtQ6i.blt HTTP/1.1\" 200 13 \"-\" \"Craftbot\""}`,
			targetField:  "log",
			expectResult: "\"GET /JBNwtQ6i.blt HTTP/1.1\" 200 13 \"-\" \"Craftbot\"",
			expr:         "JsonExtractUnescape(blob, target)",
		},
		{
			name:         "basic json extract with non existing field",
			jsonBlob:     `{"test" : "1234"}`,
			targetField:  "non_existing_field",
			expectResult: "",
			expr:         "JsonExtractUnescape(blob, target)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]interface{}{
				"blob":   test.jsonBlob,
				"target": test.targetField,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			out, err := expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, out)
		})
	}
}

func TestJsonExtractSlice(t *testing.T) {
	if err := Init(nil); err != nil {
		t.Fatal(err)
	}

	err := FileInit("testdata", "test_data_re.txt", "regex")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		jsonBlob     string
		targetField  string
		expectResult []interface{}
		expr         string
	}{
		{
			name:         "try to extract a string as a slice",
			jsonBlob:     `{"test" : "1234"}`,
			targetField:  "test",
			expectResult: nil,
			expr:         "JsonExtractSlice(blob, target)",
		},
		{
			name:         "basic json slice extract",
			jsonBlob:     `{"test" : ["1234"]}`,
			targetField:  "test",
			expectResult: []interface{}{"1234"},
			expr:         "JsonExtractSlice(blob, target)",
		},
		{
			name:         "extract with complex expression",
			jsonBlob:     `{"test": {"foo": [{"a":"b"}]}}`,
			targetField:  "test.foo",
			expectResult: []interface{}{map[string]interface{}{"a": "b"}},
			expr:         "JsonExtractSlice(blob, target)",
		},
		{
			name:         "extract non-existing key",
			jsonBlob:     `{"test: "11234"}`,
			targetField:  "foo",
			expectResult: nil,
			expr:         "JsonExtractSlice(blob, target)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]interface{}{
				"blob":   test.jsonBlob,
				"target": test.targetField,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			out, err := expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, out)
		})
	}
}

func TestJsonExtractObject(t *testing.T) {
	if err := Init(nil); err != nil {
		t.Fatal(err)
	}

	err := FileInit("testdata", "test_data_re.txt", "regex")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		jsonBlob     string
		targetField  string
		expectResult map[string]interface{}
		expr         string
	}{
		{
			name:         "try to extract a string as an object",
			jsonBlob:     `{"test" : "1234"}`,
			targetField:  "test",
			expectResult: nil,
			expr:         "JsonExtractObject(blob, target)",
		},
		{
			name:         "basic json object extract",
			jsonBlob:     `{"test" : {"1234": {"foo": "bar"}}}`,
			targetField:  "test",
			expectResult: map[string]interface{}{"1234": map[string]interface{}{"foo": "bar"}},
			expr:         "JsonExtractObject(blob, target)",
		},
		{
			name:         "extract with complex expression",
			jsonBlob:     `{"test": {"foo": [{"a":"b"}]}}`,
			targetField:  "test.foo[0]",
			expectResult: map[string]interface{}{"a": "b"},
			expr:         "JsonExtractObject(blob, target)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]interface{}{
				"blob":   test.jsonBlob,
				"target": test.targetField,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			out, err := expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, out)
		})
	}
}

func TestToJson(t *testing.T) {
	err := Init(nil)
	require.NoError(t, err)

	tests := []struct {
		name         string
		obj          interface{}
		expectResult string
		expr         string
	}{
		{
			name:         "convert int",
			obj:          42,
			expectResult: "42",
			expr:         "ToJsonString(obj)",
		},
		{
			name:         "convert slice",
			obj:          []string{"foo", "bar"},
			expectResult: `["foo","bar"]`,
			expr:         "ToJsonString(obj)",
		},
		{
			name:         "convert map",
			obj:          map[string]string{"foo": "bar"},
			expectResult: `{"foo":"bar"}`,
			expr:         "ToJsonString(obj)",
		},
		{
			name:         "convert struct",
			obj:          struct{ Foo string }{"bar"},
			expectResult: `{"Foo":"bar"}`,
			expr:         "ToJsonString(obj)",
		},
		{
			name: "convert complex struct",
			obj: struct {
				Foo string
				Bar struct {
					Baz string
				}
				Bla []string
			}{
				Foo: "bar",
				Bar: struct {
					Baz string
				}{
					Baz: "baz",
				},
				Bla: []string{"foo", "bar"},
			},
			expectResult: `{"Foo":"bar","Bar":{"Baz":"baz"},"Bla":["foo","bar"]}`,
			expr:         "ToJsonString(obj)",
		},
		{
			name:         "convert invalid type",
			obj:          func() {},
			expectResult: "",
			expr:         "ToJsonString(obj)",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := map[string]interface{}{
				"obj": test.obj,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			out, err := expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, out)
		})
	}
}

func TestUnmarshalJSON(t *testing.T) {
	err := Init(nil)
	require.NoError(t, err)

	tests := []struct {
		name         string
		json         string
		expectResult interface{}
		expr         string
	}{
		{
			name:         "convert int",
			json:         "42",
			expectResult: float64(42),
			expr:         "UnmarshalJSON(json, out, 'a')",
		},
		{
			name:         "convert slice",
			json:         `["foo","bar"]`,
			expectResult: []interface{}{"foo", "bar"},
			expr:         "UnmarshalJSON(json, out, 'a')",
		},
		{
			name:         "convert map",
			json:         `{"foo":"bar"}`,
			expectResult: map[string]interface{}{"foo": "bar"},
			expr:         "UnmarshalJSON(json, out, 'a')",
		},
		{
			name:         "convert struct",
			json:         `{"Foo":"bar"}`,
			expectResult: map[string]interface{}{"Foo": "bar"},
			expr:         "UnmarshalJSON(json, out, 'a')",
		},
		{
			name: "convert complex struct",
			json: `{"Foo":"bar","Bar":{"Baz":"baz"},"Bla":["foo","bar"]}`,
			expectResult: map[string]interface{}{
				"Foo": "bar",
				"Bar": map[string]interface{}{
					"Baz": "baz",
				},
				"Bla": []interface{}{"foo", "bar"},
			},
			expr: "UnmarshalJSON(json, out, 'a')",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outMap := make(map[string]interface{})
			env := map[string]interface{}{
				"json": test.json,
				"out":  outMap,
			}
			vm, err := expr.Compile(test.expr, GetExprOptions(env)...)
			require.NoError(t, err)
			_, err = expr.Run(vm, env)
			require.NoError(t, err)
			assert.Equal(t, test.expectResult, outMap["a"])
		})
	}
}
