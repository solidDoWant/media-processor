package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/invopop/jsonschema"

	"github.com/solidDoWant/media-processor/internal/watcherconfig"
)

func main() {
	r := &jsonschema.Reflector{
		FieldNameTag: "yaml",
	}

	if err := r.AddGoComments("github.com/solidDoWant/media-processor", "./internal/watcherconfig"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to load Go comments: %v\n", err)
		os.Exit(1)
	}

	schema := r.Reflect(&watcherconfig.Config{})

	out, err := json.Marshal(schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal schema: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(string(out))
}
