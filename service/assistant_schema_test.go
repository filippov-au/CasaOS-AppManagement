package service

import (
	stdjson "encoding/json"
	"github.com/xeipuuv/gojsonschema"
	"testing"
)

func TestAssistantToolSchemasAreValidJSONSchema(t *testing.T) {
	for _, tool := range (AssistantRuntime{}).Tools() {
		t.Run(tool.Function.Name, func(t *testing.T) {
			// Validate the serialized schema, including Go nil slices becoming JSON null.
			data, err := stdjson.Marshal(tool.Function.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			loader := gojsonschema.NewSchemaLoader()
			loader.Validate = true
			if _, err = loader.Compile(gojsonschema.NewBytesLoader(data)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
