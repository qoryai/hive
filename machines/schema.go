package machines

import (
	"embed"
	"io/fs"
)

// SchemaBase is the start of every schema's $id, followed by the schema's file name.
// It identifies the protocol; nothing is fetched from it.
const SchemaBase = "https://qory.dev/contracts/machines/v1"

//go:embed schemas/*.schema.json
var schemas embed.FS

// Schemas contains one JSON Schema, draft 2020-12, for each request, each answer and
// the error document, plus definitions.schema.json, which the others refer to by a
// relative $ref. The file names are check-request, check-answer, make-request,
// make-answer, list-request, list-answer, log-request, log-answer, delete-request,
// delete-answer and error, each followed by .schema.json. A validator of these schemas
// must assert format. The decoders of this package refuse what the schemas refuse, and
// two more things that JSON Schema cannot state: a member name that appears twice in
// one object, and a log of more than [MaxLog] bytes as UTF-8.
var Schemas fs.FS = must(fs.Sub(schemas, "schemas"))

func must(f fs.FS, err error) fs.FS {
	if err != nil {
		panic(err)
	}
	return f
}
