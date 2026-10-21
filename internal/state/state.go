// Package state is the provisioner's state folder: the access key's secret, the record
// of the enrolment that made the key, and the instance id. The folder and each file in
// it belong to the user the provisioner runs as and grant group and others no
// permission. Each file is written to a new file, synced and renamed; a new file that a
// crash left before its rename is removed by [RemoveTemporary].
package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/qoryai/forager/accesskey"
	"github.com/qoryai/hive/internal/settings"
)

// DefaultDir is the state folder when no other is set: the image's volume.
const DefaultDir = "/var/lib/qory-provisioner"

// The files of the state folder.
const (
	// KeyFile contains the access key's secret and a line feed.
	KeyFile = "access-key-secret"
	// EnrolmentFile is the record of the enrolment, see [Enrolment].
	EnrolmentFile = "enrolment.json"
	// InstanceFile contains the instance id and a line feed.
	InstanceFile = "instance-id"
)

// The most bytes each file may contain.
const (
	maxKeyFile       = 4 << 10
	maxEnrolmentFile = 64 << 10
	maxInstanceFile  = 256
)

// Enrolment is the record of the enrolment that made the access key.
type Enrolment struct {
	// ApiaryURL is the origin of the Qory Apiary the key is enrolled with.
	ApiaryURL string
	// AccessKeyID is the access key's id, ak_ and 16 characters.
	AccessKeyID string
	// PoolID is the node pool the key is enrolled in, np_ and 16 characters.
	PoolID string
	// Pin is the list of Qory Apiary's public keys that every answer is checked under.
	Pin accesskey.Pin
}

// State is what the state folder contains.
type State struct {
	Enrolment
	// Key is the access key, which signs every request to Qory Apiary.
	Key *accesskey.Key
	// InstanceID is the provisioner's instance id, i_ and 26 characters: not a secret.
	InstanceID string
}

// record is enrolment.json.
type record struct {
	Version     int             `json:"version"`
	ApiaryURL   string          `json:"apiary_url"`
	AccessKeyID string          `json:"access_key_id"`
	PoolID      string          `json:"pool_id"`
	Pin         json.RawMessage `json:"apiary_public_key"`
}

// poolIDShape is a node pool's id.
var poolIDShape = regexp.MustCompile(`^np_[0-9a-hjkmnp-tv-z]{16}$`)

// CheckDir makes the state folder at dir, 0700, when it does not exist and its parent
// does, and checks it: a symbolic link, a file that is not a folder, a folder that
// grants group or others any permission, or one that another user owns is refused, and
// so is a path whose last part is . or .. .
func CheckDir(dir string) error {
	clean := strings.TrimRight(dir, "/")
	if clean == "" && dir != "" {
		clean = "/"
	}
	// A last part . or .. names the folder through another, a link's target among
	// them, so link/. would be checked as what link points to.
	if base := filepath.Base(clean); base == "." || base == ".." {
		return fmt.Errorf("Set the state folder to the folder itself: %s ends in . or ..", dir)
	}
	if err := os.Mkdir(clean, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("Create the state folder %s: %w", dir, err)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("Create the state folder %s: %w", dir, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("Set the state folder to the folder itself: %s is a symbolic link.", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("Remove the file %s or set another state folder: it is not a folder.", dir)
	}
	return owned(info, "the state folder "+dir, "0700")
}

// owned refuses a file or folder that grants group or others any permission, saying to
// make it mode, or that another user than the one the provisioner runs as owns.
// subject names it in the error.
func owned(info fs.FileInfo, subject, mode string) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Make %s %s: group or others have permissions on it (%s).", subject, mode, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("Make %s belong to the user the provisioner runs as, uid %d: another user owns it.", subject, os.Geteuid())
	}
	return nil
}

// HasKey reports whether the state folder dir, which [CheckDir] took, has a key file.
// The key file is written last, so a folder without one holds no finished enrolment.
func HasKey(dir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dir, KeyFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, opError(dir, "Make the state folder "+dir+" readable", "checking for "+KeyFile, err)
}

// tempShape is the name of a new file writeFile makes: a dot, the name of a state
// file, a dot and 16 hexadecimal digits.
var tempShape = regexp.MustCompile(`^\.(` + regexp.QuoteMeta(KeyFile) + `|` + regexp.QuoteMeta(EnrolmentFile) + `|` + regexp.QuoteMeta(InstanceFile) + `)\.[0-9a-f]{16}$`)

// RemoveTemporary removes from the state folder dir, which [CheckDir] took, each new
// file that writeFile made and a crash left before its rename, so the secret in a new
// key file does not stay in the folder. A folder of such a name is left as it is.
func RemoveTemporary(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return opError(dir, "Make the state folder "+dir+" readable", "listing the state folder", err)
	}
	for _, e := range entries {
		if e.IsDir() || !tempShape.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return opError(dir, "Make the state folder "+dir+" writable", "removing "+e.Name(), err)
		}
	}
	return nil
}

// opError is the error of the operation op in the state folder dir that failed with
// err. A permission error says perm, such as "Make the state folder /x writable", for
// the uid the provisioner runs as; any other error says op and err.
func opError(dir, perm, op string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%s by uid %d: %w", perm, os.Geteuid(), err)
	}
	return fmt.Errorf("Fix the state folder %s, then start the provisioner again: %s failed: %w", dir, op, err)
}

// Save writes the enrolment e and its key to the state folder dir, which [CheckDir]
// took: enrolment.json first, then the key file, so a key file is there only when the
// record is. No error contains the secret.
func Save(dir string, e Enrolment, key *accesskey.Key) error {
	pin, err := json.Marshal(e.Pin)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(record{Version: 1, ApiaryURL: e.ApiaryURL, AccessKeyID: e.AccessKeyID, PoolID: e.PoolID, Pin: pin}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(dir, EnrolmentFile, append(b, '\n')); err != nil {
		return err
	}
	return writeFile(dir, KeyFile, []byte(key.Secret()+"\n"))
}

// Load reads the state folder dir, which [CheckDir] took and whose [HasKey] is true:
// the key, the enrolment, and the instance id, which it makes and writes when the
// folder has none. A state whose enrolment was made for another Qory Apiary than
// apiaryURL is refused. No error contains the secret or quotes a file's content.
func Load(dir, apiaryURL string) (State, error) {
	b, err := readFile(dir, KeyFile, maxKeyFile, "an access key secret")
	if err != nil {
		return State{}, err
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	key, err := accesskey.ParseSecret(string(b))
	if err != nil || key.PublicKey().Fixture() {
		return State{}, damaged(dir, KeyFile+" does not contain an access key secret.")
	}
	e, err := loadEnrolment(dir)
	if err != nil {
		return State{}, err
	}
	if e.ApiaryURL != apiaryURL {
		return State{}, fmt.Errorf("Set apiary_url in the settings file to %s, or empty the state folder %s and enrol again with a new code: the access key in the state folder is enrolled with %s, and apiary_url in the settings file is %s.", e.ApiaryURL, dir, e.ApiaryURL, apiaryURL)
	}
	id, err := instanceID(dir)
	if err != nil {
		return State{}, err
	}
	return State{Enrolment: e, Key: key, InstanceID: id}, nil
}

// damaged is the error of a state folder dir whose file does not contain what it
// should; what says which, and never quotes the file.
func damaged(dir, what string) error {
	return fmt.Errorf("Empty the state folder %s and enrol again with a new code, and have a workspace admin of Qory Apiary revoke the access key that was enrolled: %s", dir, what)
}

// loadEnrolment reads enrolment.json: one JSON object of version 1, each member once
// and known, each value as [Save] writes it.
func loadEnrolment(dir string) (Enrolment, error) {
	b, err := readFile(dir, EnrolmentFile, maxEnrolmentFile, "an enrolment record")
	if errors.Is(err, fs.ErrNotExist) {
		return Enrolment{}, damaged(dir, EnrolmentFile+" is missing.")
	}
	if err != nil {
		return Enrolment{}, err
	}
	notRecord := damaged(dir, EnrolmentFile+" is not an enrolment record.")
	var r record
	if accesskey.ContainsSecret(string(b)) || jsonv2.Unmarshal(b, &r, jsonv2.RejectUnknownMembers(true)) != nil || r.Version != 1 {
		return Enrolment{}, notRecord
	}
	if o, err := settings.CheckOrigin(r.ApiaryURL); err != nil || o != r.ApiaryURL {
		return Enrolment{}, notRecord
	}
	if accesskey.CheckID(r.AccessKeyID) != nil || !poolIDShape.MatchString(r.PoolID) {
		return Enrolment{}, notRecord
	}
	pin, err := accesskey.ParsePin(r.Pin)
	if err != nil || pin.Fixture() {
		return Enrolment{}, notRecord
	}
	return Enrolment{ApiaryURL: r.ApiaryURL, AccessKeyID: r.AccessKeyID, PoolID: r.PoolID, Pin: pin}, nil
}

// instanceID is the instance id in the state folder dir. A missing file, or one within
// 256 bytes that contains no instance id, is written anew with [accesskey.NewInstanceID],
// so the provisioner is the same instance after a restart.
func instanceID(dir string) (string, error) {
	b, err := readFile(dir, InstanceFile, maxInstanceFile, "an instance id")
	switch {
	case err == nil:
		id := strings.TrimSuffix(string(b), "\n")
		if strings.HasPrefix(id, accesskey.InstancePrefix) && accesskey.CheckInstanceID(id) == nil {
			return id, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	id, err := accesskey.NewInstanceID()
	if err != nil {
		return "", err
	}
	if err := writeFile(dir, InstanceFile, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}

// readFile reads the file name in the state folder dir, never through a symbolic link
// and without waiting on a pipe, and refuses a file that is not a regular one, that
// grants group or others any permission, that another user owns, or that is larger than
// max bytes. holds is what the file contains, as the error for a larger file says it.
// A missing file is an error that matches [fs.ErrNotExist]. No error quotes the file.
func readFile(dir, name string, max int, holds string) ([]byte, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, syscall.ELOOP):
		return nil, fmt.Errorf("Replace the symbolic link %s with the file itself.", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil, err
	case err != nil:
		return nil, opError(dir, "Make "+path+" readable", "opening "+name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, opError(dir, "Make "+path+" readable", "reading the mode of "+name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("Remove %s from the state folder: it is not a regular file.", path)
	}
	if err := owned(info, path, "0600"); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, opError(dir, "Make "+path+" readable", "reading "+name, err)
	}
	if len(b) > max {
		return nil, damaged(dir, name+" is larger than "+holds+".")
	}
	return b, nil
}

// writeFile writes b to the file name in the state folder dir: to a new file of a
// random name, 0600, which is synced and closed, then renamed over name, and the folder
// is synced. A write that fails before the rename leaves name as it was and removes the
// new file, unless that removal fails too; a new file left by a failed removal or a
// crash is removed by [RemoveTemporary] at the next start. A filesystem that cannot
// sync a file, see [syncUnsupported], keeps the file unsynced. No error contains b.
func writeFile(dir, name string, b []byte) error {
	random := make([]byte, 8)
	rand.Read(random)
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(random))
	writable := "Make the state folder " + dir + " writable"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return opError(dir, writable, "creating a new "+name, err)
	}
	op := "writing a new " + name
	_, err = f.Write(b)
	if err == nil {
		op = "syncing a new " + name
		if err = f.Sync(); syncUnsupported(err) {
			err = nil
		}
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		op, err = "closing a new "+name, cerr
	}
	if err == nil {
		op = "renaming a new file to " + name
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return opError(dir, writable, op, err)
	}
	// The folder's entry for the file is synced where the system can sync a folder;
	// where it cannot, the file itself is on disk all the same.
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// syncUnsupported reports whether a file's sync failed because its filesystem cannot
// sync a file, as some FUSE and network filesystems answer: EINVAL, ENOTSUP, EOPNOTSUPP
// or ENOSYS.
func syncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS)
}
