package machines

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"time"
	"unicode/utf8"
)

// The rules that a document fails. No error of this package contains a value of the
// document, so an error never shows the start data or a secret.
var (
	errNotObject = errors.New("the document must be one JSON object in UTF-8, with no member name twice")
	errUnknown   = errors.New("the document has a member that version 1 does not define")
)

// Document is a request, an answer or the error document of version 1.
type Document interface {
	// Validate returns the first rule of version 1 that the document fails, or nil.
	Validate() error
}

// Encode returns doc as one line of JSON, after [Document.Validate] passes. Its error
// states the rule that failed and contains no value of doc.
func Encode(doc Document) ([]byte, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, errors.New("the document cannot be written as JSON")
	}
	return b, nil
}

// object is one JSON object, by member name.
type object map[string]jsontext.Value

// parse reads b as one JSON object.
func parse(b []byte) (object, error) {
	v := jsontext.Value(b)
	if !v.IsValid() || v.Kind() != '{' {
		return nil, errNotObject
	}
	var o object
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, errNotObject
	}
	return o, nil
}

// only returns errUnknown when o has a member other than names.
func (o object) only(names ...string) error {
	n := 0
	for _, name := range names {
		if _, ok := o[name]; ok {
			n++
		}
	}
	if n != len(o) {
		return errUnknown
	}
	return nil
}

// member is the member name of o, of the JSON kind k.
func (o object) member(name string, k jsontext.Kind, what string) (jsontext.Value, error) {
	v, ok := o[name]
	if !ok {
		return nil, fmt.Errorf("the member %s is missing", name)
	}
	if v.Kind() != k {
		return nil, fmt.Errorf("the member %s must be %s", name, what)
	}
	return v, nil
}

// str is the string member name of o.
func (o object) str(name string) (string, error) {
	v, err := o.member(name, '"', "a string")
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("the member %s must be a string", name)
	}
	return s, nil
}

// version reads the member version of o, which must be the number 1.
func (o object) version() (int, error) {
	v, err := o.member("version", '0', "the number 1")
	if err != nil {
		return 0, err
	}
	n, ok := new(big.Rat).SetString(string(v))
	if !ok || n.Cmp(big.NewRat(Version, 1)) != 0 {
		return 0, errors.New("the member version must be 1")
	}
	return Version, nil
}

// request parses b as a request with the members names besides version.
func request(b []byte, names ...string) (object, error) {
	o, err := parse(b)
	if err != nil {
		return nil, err
	}
	if err := o.only(append(names, "version")...); err != nil {
		return nil, err
	}
	if _, err := o.version(); err != nil {
		return nil, err
	}
	return o, nil
}

// The rules of single values, shared by the decoders and Validate.

func checkVersion(v int) error {
	if v != Version {
		return errors.New("the member version must be 1")
	}
	return nil
}

func checkKey(s string) error {
	if !keyShape.MatchString(s) {
		return errors.New("the member key must be dp_ and 16 characters of 0-9 and a-z without i, l, o and u")
	}
	return nil
}

func checkPool(s string) error {
	if !poolShape.MatchString(s) {
		return errors.New("the member pool must be np_ and 16 characters of 0-9 and a-z without i, l, o and u")
	}
	return nil
}

func checkSize(s string) error {
	if !sizeShape.MatchString(s) {
		return errors.New("the member size must be 1 to 64 characters of a-z, 0-9 and -, starting with a letter")
	}
	return nil
}

func checkID(s string) error {
	if !idShape.MatchString(s) || s == NoID {
		return errors.New("the member id must be 1 to 128 ASCII characters from ! to ~, and not - alone")
	}
	return nil
}

func checkStartData(b []byte) error {
	if len(b) < 1 || len(b) > MaxStartData {
		return errors.New("the member start_data must be standard base64 of 1 to 16384 bytes")
	}
	return nil
}

func checkStarted(s string) error {
	if !startedShape.MatchString(s) {
		return errors.New("the member started must be an RFC 3339 time in UTC, such as 2026-10-22T10:00:00Z")
	}
	if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
		return errors.New("the member started must be an RFC 3339 time in UTC, such as 2026-10-22T10:00:00Z")
	}
	return nil
}

func checkListState(s State) error {
	switch s {
	case StateStarting, StateRunning, StateStopped, StateDeleting:
		return nil
	}
	return errors.New("the member state must be starting, running, stopped or deleting")
}

func checkDeleteState(s State) error {
	switch s {
	case StateDeleting, StateGone:
		return nil
	}
	return errors.New("the member state must be deleting or gone")
}

func checkLog(s string) error {
	if len(s) > MaxLog || !utf8.ValidString(s) {
		return errors.New("the member log must be at most 786432 bytes of UTF-8")
	}
	return nil
}

func checkKind(k Kind) error {
	switch k {
	case KindLimit, KindRetry, KindDefect, KindOther:
		return nil
	}
	return errors.New("the member kind must be limit, retry, defect or other")
}

func checkMessage(s string) error {
	if n := utf8.RuneCountInString(s); n < 1 || n > MaxMessage || !utf8.ValidString(s) {
		return errors.New("the member message must be 1 to 4096 characters of UTF-8")
	}
	return nil
}

// Validate returns the first rule that r fails, or nil.
func (r CheckRequest) Validate() error { return checkVersion(r.Version) }

// Validate returns the first rule that r fails, or nil.
func (r MakeRequest) Validate() error {
	if err := checkVersion(r.Version); err != nil {
		return err
	}
	if err := checkKey(r.Key); err != nil {
		return err
	}
	if err := checkPool(r.Pool); err != nil {
		return err
	}
	if err := checkSize(r.Size); err != nil {
		return err
	}
	return checkStartData(r.StartData)
}

// Validate returns the first rule that r fails, or nil.
func (r ListRequest) Validate() error {
	if err := checkVersion(r.Version); err != nil {
		return err
	}
	return checkPool(r.Pool)
}

// Validate returns the first rule that r fails, or nil.
func (r LogRequest) Validate() error {
	if err := checkVersion(r.Version); err != nil {
		return err
	}
	return checkID(r.ID)
}

// Validate returns the first rule that r fails, or nil.
func (r DeleteRequest) Validate() error {
	if err := checkVersion(r.Version); err != nil {
		return err
	}
	return checkID(r.ID)
}

// Validate returns nil: the empty object passes every rule.
func (CheckAnswer) Validate() error { return nil }

// Validate returns the first rule that a fails, or nil.
func (a MakeAnswer) Validate() error { return checkID(a.ID) }

// Validate returns the first rule that a fails, or nil. A rule of a machine starts
// with its place, machines[0] for the first.
func (a ListAnswer) Validate() error {
	for i, m := range a.Machines {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("machines[%d]: %w", i, err)
		}
	}
	return nil
}

// Validate returns the first rule that m fails, or nil.
func (m Machine) Validate() error {
	if err := checkID(m.ID); err != nil {
		return err
	}
	if err := checkKey(m.Key); err != nil {
		return err
	}
	if err := checkListState(m.State); err != nil {
		return err
	}
	return checkStarted(m.Started.UTC().Format(time.RFC3339Nano))
}

// Validate returns the first rule that a fails, or nil.
func (a LogAnswer) Validate() error { return checkLog(a.Log) }

// Validate returns the first rule that a fails, or nil.
func (a DeleteAnswer) Validate() error { return checkDeleteState(a.State) }

// Validate returns the first rule that e fails, or nil.
func (e Error) Validate() error {
	if err := checkKind(e.Kind); err != nil {
		return err
	}
	return checkMessage(e.Message)
}

// Validate returns the first rule that a fails, or nil. A rule of the failure starts
// with error.
func (a ErrorAnswer) Validate() error {
	if err := a.Error.Validate(); err != nil {
		return fmt.Errorf("error: %w", err)
	}
	return nil
}

// DecodeCheckRequest reads a check request. Its error states the first rule that b
// fails and contains no value of b.
func DecodeCheckRequest(b []byte) (CheckRequest, error) {
	if _, err := request(b); err != nil {
		return CheckRequest{}, err
	}
	return CheckRequest{Version: Version}, nil
}

// DecodeMakeRequest reads a make request. Its error states the first rule that b fails
// and contains no value of b.
func DecodeMakeRequest(b []byte) (MakeRequest, error) {
	o, err := request(b, "key", "pool", "size", "start_data")
	if err != nil {
		return MakeRequest{}, err
	}
	r := MakeRequest{Version: Version}
	if r.Key, err = o.str("key"); err != nil {
		return MakeRequest{}, err
	}
	if r.Pool, err = o.str("pool"); err != nil {
		return MakeRequest{}, err
	}
	if r.Size, err = o.str("size"); err != nil {
		return MakeRequest{}, err
	}
	data, err := o.str("start_data")
	if err != nil {
		return MakeRequest{}, err
	}
	if !startDataShape.MatchString(data) {
		return MakeRequest{}, checkStartData(nil)
	}
	if r.StartData, err = base64.StdEncoding.Strict().DecodeString(data); err != nil {
		return MakeRequest{}, checkStartData(nil)
	}
	if err := r.Validate(); err != nil {
		return MakeRequest{}, err
	}
	return r, nil
}

// DecodeListRequest reads a list request. Its error states the first rule that b fails
// and contains no value of b.
func DecodeListRequest(b []byte) (ListRequest, error) {
	o, err := request(b, "pool")
	if err != nil {
		return ListRequest{}, err
	}
	r := ListRequest{Version: Version}
	if r.Pool, err = o.str("pool"); err != nil {
		return ListRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return ListRequest{}, err
	}
	return r, nil
}

// DecodeLogRequest reads a log request. Its error states the first rule that b fails
// and contains no value of b.
func DecodeLogRequest(b []byte) (LogRequest, error) {
	o, err := request(b, "id")
	if err != nil {
		return LogRequest{}, err
	}
	r := LogRequest{Version: Version}
	if r.ID, err = o.str("id"); err != nil {
		return LogRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return LogRequest{}, err
	}
	return r, nil
}

// DecodeDeleteRequest reads a delete request. Its error states the first rule that b
// fails and contains no value of b.
func DecodeDeleteRequest(b []byte) (DeleteRequest, error) {
	o, err := request(b, "id")
	if err != nil {
		return DeleteRequest{}, err
	}
	r := DeleteRequest{Version: Version}
	if r.ID, err = o.str("id"); err != nil {
		return DeleteRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return DeleteRequest{}, err
	}
	return r, nil
}

// DecodeCheckAnswer reads a check answer. Its error states the first rule that b fails
// and contains no value of b.
func DecodeCheckAnswer(b []byte) (CheckAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return CheckAnswer{}, err
	}
	if err := o.only(); err != nil {
		return CheckAnswer{}, err
	}
	return CheckAnswer{}, nil
}

// DecodeMakeAnswer reads a make answer. Its error states the first rule that b fails
// and contains no value of b.
func DecodeMakeAnswer(b []byte) (MakeAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return MakeAnswer{}, err
	}
	if err := o.only("id"); err != nil {
		return MakeAnswer{}, err
	}
	var a MakeAnswer
	if a.ID, err = o.str("id"); err != nil {
		return MakeAnswer{}, err
	}
	if err := a.Validate(); err != nil {
		return MakeAnswer{}, err
	}
	return a, nil
}

// DecodeListAnswer reads a list answer. Its error states the first rule that b fails
// and contains no value of b. A rule of a machine starts with its place, machines[0]
// for the first.
func DecodeListAnswer(b []byte) (ListAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return ListAnswer{}, err
	}
	if err := o.only("machines"); err != nil {
		return ListAnswer{}, err
	}
	v, err := o.member("machines", '[', "an array")
	if err != nil {
		return ListAnswer{}, err
	}
	var items []jsontext.Value
	if err := json.Unmarshal(v, &items); err != nil {
		return ListAnswer{}, errors.New("the member machines must be an array")
	}
	a := ListAnswer{Machines: make([]Machine, 0, len(items))}
	for i, item := range items {
		m, err := decodeMachine(item)
		if err != nil {
			return ListAnswer{}, fmt.Errorf("machines[%d]: %w", i, err)
		}
		a.Machines = append(a.Machines, m)
	}
	return a, nil
}

// decodeMachine reads one machine of a list answer.
func decodeMachine(b []byte) (Machine, error) {
	o, err := parse(b)
	if err != nil {
		return Machine{}, errors.New("each machine must be a JSON object")
	}
	if err := o.only("id", "key", "state", "started"); err != nil {
		return Machine{}, err
	}
	var m Machine
	if m.ID, err = o.str("id"); err != nil {
		return Machine{}, err
	}
	if err := checkID(m.ID); err != nil {
		return Machine{}, err
	}
	if m.Key, err = o.str("key"); err != nil {
		return Machine{}, err
	}
	if err := checkKey(m.Key); err != nil {
		return Machine{}, err
	}
	state, err := o.str("state")
	if err != nil {
		return Machine{}, err
	}
	m.State = State(state)
	if err := checkListState(m.State); err != nil {
		return Machine{}, err
	}
	started, err := o.str("started")
	if err != nil {
		return Machine{}, err
	}
	if err := checkStarted(started); err != nil {
		return Machine{}, err
	}
	m.Started, _ = time.Parse(time.RFC3339Nano, started)
	return m, nil
}

// DecodeLogAnswer reads a log answer. Its error states the first rule that b fails and
// contains no value of b.
func DecodeLogAnswer(b []byte) (LogAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return LogAnswer{}, err
	}
	if err := o.only("log"); err != nil {
		return LogAnswer{}, err
	}
	var a LogAnswer
	if a.Log, err = o.str("log"); err != nil {
		return LogAnswer{}, err
	}
	if err := a.Validate(); err != nil {
		return LogAnswer{}, err
	}
	return a, nil
}

// DecodeDeleteAnswer reads a delete answer. Its error states the first rule that b
// fails and contains no value of b.
func DecodeDeleteAnswer(b []byte) (DeleteAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return DeleteAnswer{}, err
	}
	if err := o.only("state"); err != nil {
		return DeleteAnswer{}, err
	}
	state, err := o.str("state")
	if err != nil {
		return DeleteAnswer{}, err
	}
	a := DeleteAnswer{State: State(state)}
	if err := a.Validate(); err != nil {
		return DeleteAnswer{}, err
	}
	return a, nil
}

// DecodeErrorAnswer reads an error document. Its error states the first rule that b
// fails and contains no value of b. A rule of the failure starts with error.
func DecodeErrorAnswer(b []byte) (ErrorAnswer, error) {
	o, err := parse(b)
	if err != nil {
		return ErrorAnswer{}, err
	}
	if err := o.only("error"); err != nil {
		return ErrorAnswer{}, err
	}
	v, err := o.member("error", '{', "an object")
	if err != nil {
		return ErrorAnswer{}, err
	}
	e, err := parse(v)
	if err != nil {
		return ErrorAnswer{}, fmt.Errorf("error: %w", err)
	}
	if err := e.only("kind", "message"); err != nil {
		return ErrorAnswer{}, fmt.Errorf("error: %w", err)
	}
	var a ErrorAnswer
	kind, err := e.str("kind")
	if err != nil {
		return ErrorAnswer{}, fmt.Errorf("error: %w", err)
	}
	a.Error.Kind = Kind(kind)
	if a.Error.Message, err = e.str("message"); err != nil {
		return ErrorAnswer{}, fmt.Errorf("error: %w", err)
	}
	if err := a.Validate(); err != nil {
		return ErrorAnswer{}, err
	}
	return a, nil
}
