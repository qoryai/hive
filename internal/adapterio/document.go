package adapterio

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"

	"github.com/qoryai/integrations/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// The rules that a document fails beyond a schema's rules.
var (
	errNotObject = errors.New("the document must be one JSON object in UTF-8, with no member name twice")
	errUnknown   = errors.New("the document has a member that version 1 does not define")
	errStartData = fmt.Errorf("the member start_data must be standard base64 of 1 to %d bytes, with padding bits of zero", contracts.MaxStartData)
	errLog       = fmt.Errorf("the member log must be at most %d bytes of UTF-8", MaxLog)
)

// The schema files of machines/v1, in contracts.MachinesFS.
const (
	checkFile        = "check.schema.json"
	checkAnswerFile  = "check-answer.schema.json"
	makeFile         = "make.schema.json"
	makeAnswerFile   = "make-answer.schema.json"
	listFile         = "list.schema.json"
	listAnswerFile   = "list-answer.schema.json"
	logFile          = "log.schema.json"
	logAnswerFile    = "log-answer.schema.json"
	deleteFile       = "delete.schema.json"
	deleteAnswerFile = "delete-answer.schema.json"
	errorFile        = "error.schema.json"
)

// keyDefinition is the definition of a dispatch id in the provisioner contract, which
// the key of a make request and of a listed machine refers to.
const keyDefinition = "definitions.schema.json#/$defs/dispatch_id"

// compiled are the schemas of machines/v1 by file name, and the definition of a key.
type compiled struct {
	files map[string]*jsonschema.Schema
	key   *jsonschema.Schema
}

// schemas compiles the schemas once.
var schemas = sync.OnceValues(func() (compiled, error) {
	c := compiled{files: map[string]*jsonschema.Schema{}}
	for _, f := range []string{checkFile, checkAnswerFile, makeFile, makeAnswerFile, listFile, listAnswerFile, logFile, logAnswerFile, deleteFile, deleteAnswerFile, errorFile} {
		s, err := contracts.CompileMachines(f)
		if err != nil {
			return compiled{}, fmt.Errorf("the schema machines/v1 %s does not compile: %w", f, err)
		}
		c.files[f] = s
	}
	s, err := contracts.CompileProvisioner(keyDefinition)
	if err != nil {
		return compiled{}, fmt.Errorf("the schema provisioner/v1 %s does not compile: %w", keyDefinition, err)
	}
	c.key = s
	return c, nil
})

// check returns the first rule that b fails: one JSON object in UTF-8 with no member
// name twice, then the schema file of machines/v1.
func check(file string, b []byte) error {
	v := jsontext.Value(b)
	if !v.IsValid() || v.Kind() != '{' {
		return errNotObject
	}
	c, err := schemas()
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return errNotObject
	}
	err = c.files[file].Validate(doc)
	var failed *jsonschema.ValidationError
	if errors.As(err, &failed) {
		return rule(file, failed)
	}
	return err
}

// leaf is the first error of e without causes, by instance location, then by keyword.
func leaf(e *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(e.Causes) > 0 {
		e = slices.MinFunc(e.Causes, func(a, b *jsonschema.ValidationError) int {
			return cmp.Or(
				slices.Compare(a.InstanceLocation, b.InstanceLocation),
				slices.Compare(a.ErrorKind.KeywordPath(), b.ErrorKind.KeywordPath()),
			)
		})
	}
	return e
}

// rule states the first rule of the schema file that a validation error e reports. The
// text contains member names and values of the schema, and no value of the document.
//
// The start data has one text for each rule that it fails, errStartData.
func rule(file string, e *jsonschema.ValidationError) error {
	e = leaf(e)
	at := place(e.InstanceLocation)
	if file == makeFile && at == "start_data" {
		return errStartData
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Not:
		return fmt.Errorf("the member %s has a value that the rule not of machines/v1 %s refuses", at, file)
	case *kind.Required:
		return fmt.Errorf("the member %s is missing", place(append(slices.Clone(e.InstanceLocation), k.Missing[0])))
	case *kind.AdditionalProperties:
		if at == "" {
			return errUnknown
		}
		return fmt.Errorf("%s: %w", at, errUnknown)
	case *kind.Type:
		return fmt.Errorf("the member %s must be %s", at, oneOf(k.Want, typeName))
	case *kind.Const:
		return fmt.Errorf("the member %s must be %v", at, k.Want)
	case *kind.Enum:
		return fmt.Errorf("the member %s must be %s", at, oneOf(k.Want, func(v any) string { return fmt.Sprint(v) }))
	case *kind.Pattern:
		return fmt.Errorf("the member %s must match the pattern %s", at, k.Want)
	case *kind.MinLength:
		if k.Want == 1 {
			return fmt.Errorf("the member %s must not be empty", at)
		}
		return fmt.Errorf("the member %s must have at least %d characters", at, k.Want)
	case *kind.MaxLength:
		return fmt.Errorf("the member %s must have at most %d characters", at, k.Want)
	case *kind.Format:
		return fmt.Errorf("the member %s must be in the format %s", at, k.Want)
	}
	return fmt.Errorf("the member %s fails a rule of machines/v1 %s", at, file)
}

// place writes an instance location as member names joined by dots, with an array
// index in brackets: machines[0].started.
func place(location []string) string {
	var b strings.Builder
	for _, s := range location {
		switch {
		case s != "" && strings.Trim(s, "0123456789") == "":
			b.WriteString("[" + s + "]")
		case b.Len() > 0:
			b.WriteString("." + s)
		default:
			b.WriteString(s)
		}
	}
	return b.String()
}

// oneOf writes values as "a", "a or b", or "a, b or c".
func oneOf[T any](values []T, text func(T) string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = text(v)
	}
	if len(out) < 2 {
		return strings.Join(out, "")
	}
	return strings.Join(out[:len(out)-1], ", ") + " or " + out[len(out)-1]
}

// typeName is a JSON Schema type with its article.
func typeName(t string) string {
	switch t {
	case "object", "array", "integer":
		return "an " + t
	case "null":
		return t
	}
	return "a " + t
}

// wholeNumbers reads a JSON number such as 1, 1.0 or 1e0 into an int when its value is
// a whole number. The schemas compare numbers by value, so the decoders do too.
var wholeNumbers = json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, n *int) error {
	v, err := dec.ReadValue()
	if err != nil {
		return err
	}
	r, ok := new(big.Rat).SetString(string(v))
	if v.Kind() != '0' || !ok || !r.IsInt() || !r.Num().IsInt64() {
		return errors.New("the number is not a whole number")
	}
	*n = int(r.Num().Int64())
	return nil
}))

// decode checks b against the schema file and reads it into a T.
func decode[T any](file string, b []byte) (T, error) {
	var v T
	if err := check(file, b); err != nil {
		return v, err
	}
	if err := json.Unmarshal(b, &v, wholeNumbers); err != nil {
		var zero T
		return zero, fmt.Errorf("the document passes machines/v1 %s and cannot be read into its Go type", file)
	}
	return v, nil
}

// startData decodes a make request's start data, of 1 to contracts.MaxStartData bytes
// with padding bits of zero.
func startData(s string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) < 1 || len(b) > contracts.MaxStartData {
		return nil, errStartData
	}
	return b, nil
}

// checkLog returns errLog for a log of more than MaxLog bytes.
func checkLog(log string) error {
	if len(log) > MaxLog {
		return errLog
	}
	return nil
}

// CheckKey returns an error when key is not a dispatch id, the shape of the key of a
// make request and of a listed machine. The error contains no value of key.
func CheckKey(key string) error {
	c, err := schemas()
	if err != nil {
		return err
	}
	if c.key.Validate(key) != nil {
		return errors.New("the key must be dp_ and 16 characters of 0-9 and a-z without i, l, o and u")
	}
	return nil
}

// Encode returns doc, a request, an answer or the error document of machines/v1, as one
// line of JSON, after doc passes the rules of its decoder. doc is a value of a type of
// package contracts, such as contracts.MachinesMake. Its error states the rule that
// failed and contains no value of doc.
func Encode(doc any) ([]byte, error) {
	var file string
	extra := func() error { return nil }
	switch d := doc.(type) {
	case contracts.MachinesCheck:
		file = checkFile
	case contracts.MachinesCheckAnswer:
		file = checkAnswerFile
	case contracts.MachinesMake:
		file = makeFile
		extra = func() error { _, err := startData(d.StartData); return err }
	case contracts.MachinesMakeAnswer:
		file = makeAnswerFile
	case contracts.MachinesList:
		file = listFile
	case contracts.MachinesListAnswer:
		file = listAnswerFile
	case contracts.MachinesLog:
		file = logFile
	case contracts.MachinesLogAnswer:
		file = logAnswerFile
		extra = func() error { return checkLog(d.Log) }
	case contracts.MachinesDelete:
		file = deleteFile
	case contracts.MachinesDeleteAnswer:
		file = deleteAnswerFile
	case contracts.MachinesError:
		file = errorFile
	default:
		return nil, fmt.Errorf("the document's type %T is not a document of machines/v1", doc)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, errors.New("the document cannot be written as JSON in UTF-8")
	}
	if err := check(file, b); err != nil {
		return nil, err
	}
	if err := extra(); err != nil {
		return nil, err
	}
	return b, nil
}

// DecodeCheck reads a check request. Its error states the first rule that b fails and
// contains no value of b.
func DecodeCheck(b []byte) (contracts.MachinesCheck, error) {
	return decode[contracts.MachinesCheck](checkFile, b)
}

// DecodeMake reads a make request, and returns its start data decoded. Its error
// states the first rule that b fails and contains no value of b.
func DecodeMake(b []byte) (contracts.MachinesMake, []byte, error) {
	r, err := decode[contracts.MachinesMake](makeFile, b)
	if err != nil {
		return contracts.MachinesMake{}, nil, err
	}
	data, err := startData(r.StartData)
	if err != nil {
		return contracts.MachinesMake{}, nil, err
	}
	return r, data, nil
}

// DecodeList reads a list request. Its error states the first rule that b fails and
// contains no value of b.
func DecodeList(b []byte) (contracts.MachinesList, error) {
	return decode[contracts.MachinesList](listFile, b)
}

// DecodeLog reads a log request. Its error states the first rule that b fails and
// contains no value of b.
func DecodeLog(b []byte) (contracts.MachinesLog, error) {
	return decode[contracts.MachinesLog](logFile, b)
}

// DecodeDelete reads a delete request. Its error states the first rule that b fails
// and contains no value of b.
func DecodeDelete(b []byte) (contracts.MachinesDelete, error) {
	return decode[contracts.MachinesDelete](deleteFile, b)
}

// DecodeCheckAnswer reads a check answer. Its error states the first rule that b fails
// and contains no value of b.
func DecodeCheckAnswer(b []byte) (contracts.MachinesCheckAnswer, error) {
	return decode[contracts.MachinesCheckAnswer](checkAnswerFile, b)
}

// DecodeMakeAnswer reads a make answer. Its error states the first rule that b fails
// and contains no value of b.
func DecodeMakeAnswer(b []byte) (contracts.MachinesMakeAnswer, error) {
	return decode[contracts.MachinesMakeAnswer](makeAnswerFile, b)
}

// DecodeListAnswer reads a list answer. Its error states the first rule that b fails
// and contains no value of b. A rule of a machine starts with its place, machines[0]
// for the first.
func DecodeListAnswer(b []byte) (contracts.MachinesListAnswer, error) {
	return decode[contracts.MachinesListAnswer](listAnswerFile, b)
}

// DecodeLogAnswer reads a log answer. Its error states the first rule that b fails and
// contains no value of b.
func DecodeLogAnswer(b []byte) (contracts.MachinesLogAnswer, error) {
	a, err := decode[contracts.MachinesLogAnswer](logAnswerFile, b)
	if err != nil {
		return contracts.MachinesLogAnswer{}, err
	}
	if err := checkLog(a.Log); err != nil {
		return contracts.MachinesLogAnswer{}, err
	}
	return a, nil
}

// DecodeDeleteAnswer reads a delete answer. Its error states the first rule that b
// fails and contains no value of b.
func DecodeDeleteAnswer(b []byte) (contracts.MachinesDeleteAnswer, error) {
	return decode[contracts.MachinesDeleteAnswer](deleteAnswerFile, b)
}

// DecodeError reads an error document. Its error states the first rule that b fails
// and contains no value of b. A rule of the failure names error.kind or error.message.
func DecodeError(b []byte) (contracts.MachinesError, error) {
	return decode[contracts.MachinesError](errorFile, b)
}
