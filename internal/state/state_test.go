package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qoryai/forager/accesskey"
)

// newKey is a new access key.
func newKey(t *testing.T) *accesskey.Key {
	t.Helper()
	k, err := accesskey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// enrolment is a record of an enrolment with the Qory Apiary at apiary.example.com.
func enrolment(t *testing.T) Enrolment {
	t.Helper()
	server := newKey(t)
	return Enrolment{
		ApiaryURL:   "https://apiary.example.com",
		AccessKeyID: "ak_0123456789abcdef",
		PoolID:      "np_0123456789abcdef",
		Pin:         accesskey.Pin{{Alg: "ed25519", PublicKey: server.PublicKey().String()}},
	}
}

// stateDir is a state folder that does not exist yet, in a folder that does.
func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "qory-provisioner")
}

// saved is a state folder with an enrolment and its key saved, and the key.
func saved(t *testing.T) (string, Enrolment, *accesskey.Key) {
	t.Helper()
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	e, k := enrolment(t), newKey(t)
	if err := Save(dir, e, k); err != nil {
		t.Fatal(err)
	}
	return dir, e, k
}

func TestCheckDirMakesTheFolder0700(t *testing.T) {
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", info, err)
	}
	if err := CheckDir(dir + "/"); err != nil {
		t.Errorf("a trailing slash: %v", err)
	}
}

func TestCheckDirRefusesAFolderOthersCanUse(t *testing.T) {
	for _, mode := range []fs.FileMode{0o750, 0o705, 0o770, 0o707, 0o701, 0o710, 0o777} {
		dir := stateDir(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		err := CheckDir(dir)
		want := "Make the state folder " + dir + " 0700: group or others have permissions on it (" + mode.String() + ")."
		if err == nil || err.Error() != want {
			t.Errorf("%v: %v, want %q", mode, err, want)
		}
	}
}

func TestCheckDirRefusesWhatIsNoFolder(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		file:                                 "Remove the file " + file + " or set another state folder: it is not a folder.",
		link:                                 "Set the state folder to the folder itself: " + link + " is a symbolic link.",
		link + "/":                           "Set the state folder to the folder itself: " + link + "/ is a symbolic link.",
		target + "/..":                       "Set the state folder to the folder itself: " + target + "/.. ends in . or ..",
		filepath.Join(parent, "none", "sub"): "Create the state folder " + filepath.Join(parent, "none", "sub") + ": mkdir ",
	} {
		err := CheckDir(path)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v, want %q", path, err, want)
		}
	}
}

// fakeInfo is the information of a file that another user owns.
type fakeInfo struct {
	fs.FileInfo
	uid uint32
}

func (f fakeInfo) Sys() any { return &syscall.Stat_t{Uid: f.uid} }

func TestAFileAnotherUserOwnsIsRefused(t *testing.T) {
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := uint32(os.Geteuid() + 1)
	err = owned(fakeInfo{info, other}, "the state folder "+dir, "0700")
	want := "Make the state folder " + dir + " belong to the user the provisioner runs as, uid "
	if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), ": another user owns it.") {
		t.Errorf("%v", err)
	}
}

// TestSaveWritesTheFilesAndLoadReadsThem pins the round trip: the key file 0600 with
// the secret, enrolment.json without it, and an instance id that Load makes once and
// keeps.
func TestSaveWritesTheFilesAndLoadReadsThem(t *testing.T) {
	dir, e, k := saved(t)
	has, err := HasKey(dir)
	if err != nil || !has {
		t.Fatalf("%v %v", has, err)
	}
	st, err := Load(dir, e.ApiaryURL)
	if err != nil {
		t.Fatal(err)
	}
	if st.Key.Secret() != k.Secret() || st.ApiaryURL != e.ApiaryURL || st.AccessKeyID != e.AccessKeyID || st.PoolID != e.PoolID ||
		len(st.Pin) != 1 || st.Pin[0] != e.Pin[0] || !strings.HasPrefix(st.InstanceID, accesskey.InstancePrefix) || accesskey.CheckInstanceID(st.InstanceID) != nil {
		t.Errorf("%+v", st)
	}
	again, err := Load(dir, e.ApiaryURL)
	if err != nil || again.InstanceID != st.InstanceID {
		t.Errorf("the instance id changed: %q %q %v", st.InstanceID, again.InstanceID, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
			t.Errorf("%s: %v %v", entry.Name(), info.Mode(), err)
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if entry.Name() == KeyFile {
			if string(b) != k.Secret()+"\n" {
				t.Errorf("the key file is %d bytes, not the secret", len(b))
			}
		} else if strings.Contains(string(b), k.Secret()) || strings.Contains(string(b), accesskey.SecretPrefix) {
			t.Errorf("%s contains the secret", entry.Name())
		}
	}
	if strings.Join(names, " ") != "access-key-secret enrolment.json instance-id" {
		t.Errorf("the state folder has %q", names)
	}
}

func TestLoadRefusesAStateOfAnotherQoryApiary(t *testing.T) {
	dir, _, _ := saved(t)
	_, err := Load(dir, "https://other.example.com")
	want := "Set apiary_url in the settings file to https://apiary.example.com, or empty the state folder " + dir +
		" and enrol again with a new code: the access key in the state folder is enrolled with https://apiary.example.com, and apiary_url in the settings file is https://other.example.com."
	if err == nil || err.Error() != want {
		t.Errorf("%v", err)
	}
}

func TestHasKeyIsFalseForANewFolder(t *testing.T) {
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	if has, err := HasKey(dir); err != nil || has {
		t.Errorf("%v %v", has, err)
	}
	// enrolment.json without a key is an enrolment that did not finish: the key is
	// written last.
	if err := writeFile(dir, EnrolmentFile, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if has, err := HasKey(dir); err != nil || has {
		t.Errorf("%v %v", has, err)
	}
}

// TestLoadRefusesADamagedState pins that a key file without enrolment.json, and files
// that do not hold what they should, are refused, and no error says what a file holds.
func TestLoadRefusesADamagedState(t *testing.T) {
	const marker = "MARKER-VALUE"
	for name, tc := range map[string]struct {
		file, content, want string
	}{
		"no enrolment":     {EnrolmentFile, "", "enrolment.json is missing."},
		"empty key":        {KeyFile, "", "access-key-secret does not contain an access key secret."},
		"other key":        {KeyFile, "qak_" + marker + "\n", "access-key-secret does not contain an access key secret."},
		"large key":        {KeyFile, strings.Repeat("a", 5000), "access-key-secret is larger than an access key secret."},
		"not json":         {EnrolmentFile, marker, "enrolment.json is not an enrolment record."},
		"unknown member":   {EnrolmentFile, `{"version": 1, "x": "` + marker + `"}`, "enrolment.json is not an enrolment record."},
		"duplicate member": {EnrolmentFile, `{"version": 1, "version": 1}`, "enrolment.json is not an enrolment record."},
		"other version":    {EnrolmentFile, `{"version": 2, "apiary_url": "https://apiary.example.com", "access_key_id": "ak_0123456789abcdef", "pool_id": "np_0123456789abcdef", "apiary_public_key": []}`, "enrolment.json is not an enrolment record."},
		"bad pool":         {EnrolmentFile, `{"version": 1, "apiary_url": "https://apiary.example.com", "access_key_id": "ak_0123456789abcdef", "pool_id": "` + marker + `", "apiary_public_key": []}`, "enrolment.json is not an enrolment record."},
		"secret in record": {EnrolmentFile, `{"version": 1, "apiary_url": "https://apiary.example.com", "access_key_id": "qak_` + marker + `"}`, "enrolment.json is not an enrolment record."},
		"large instance":   {InstanceFile, strings.Repeat("a", 300), "instance-id is larger than an instance id."},
	} {
		dir, e, _ := saved(t)
		path := filepath.Join(dir, tc.file)
		if tc.content == "" && tc.file == EnrolmentFile {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir, e.ApiaryURL)
		want := "Empty the state folder " + dir + " and enrol again with a new code, and have a workspace admin of Qory Apiary revoke the access key that was enrolled: " + tc.want
		if err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
		if err != nil && strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error quotes the file: %v", name, err)
		}
	}
}

func TestLoadRefusesAFileOthersCanUse(t *testing.T) {
	for _, name := range []string{KeyFile, EnrolmentFile, InstanceFile} {
		dir, e, _ := saved(t)
		if _, err := Load(dir, e.ApiaryURL); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		_, err := Load(dir, e.ApiaryURL)
		if err == nil || err.Error() != "Make "+path+" 0600: group or others have permissions on it (-rw-r-----)." {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoadRefusesALinkOrAFolder(t *testing.T) {
	dir, e, k := saved(t)
	path := filepath.Join(dir, KeyFile)
	elsewhere := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(elsewhere, []byte(k.Secret()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, e.ApiaryURL); err == nil || err.Error() != "Replace the symbolic link "+path+" with the file itself." {
		t.Errorf("%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, e.ApiaryURL); err == nil || err.Error() != "Remove "+path+" from the state folder: it is not a regular file." {
		t.Errorf("%v", err)
	}
}

// TestLoadWritesAnInstanceIDAnew pins that a missing instance id, or one that is not an
// instance id, is made anew, and the new one is kept.
func TestLoadWritesAnInstanceIDAnew(t *testing.T) {
	for _, content := range []string{"", "x\n", "i_qak_0123\n"} {
		dir, e, _ := saved(t)
		path := filepath.Join(dir, InstanceFile)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := Load(dir, e.ApiaryURL)
		if err != nil || accesskey.CheckInstanceID(st.InstanceID) != nil || !strings.HasPrefix(st.InstanceID, accesskey.InstancePrefix) {
			t.Fatalf("%q: %q %v", content, st.InstanceID, err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != st.InstanceID+"\n" {
			t.Errorf("%q: the file has %q", content, b)
		}
	}
}

// TestWriteFileReplacesByRename pins that a file is written to a new file and renamed
// over the old one, which leaves no other file behind, and that a file the write could
// not finish leaves the old one as it was.
func TestWriteFileReplacesByRename(t *testing.T) {
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, InstanceFile)
	if err := writeFile(dir, InstanceFile, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := writeFile(dir, InstanceFile, []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the file was written in place")
	}
	if b, _ := os.ReadFile(path); string(b) != "two\n" {
		t.Errorf("%q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%d files", len(entries))
	}
	// Root ignores file modes, so root can write into a folder of mode 0500.
	if os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := writeFile(dir, InstanceFile, []byte("three\n")); err == nil || !strings.HasPrefix(err.Error(), "Make the state folder "+dir+" writable by uid "+strconv.Itoa(os.Geteuid())+": open ") {
		t.Errorf("%v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "two\n" {
		t.Errorf("%q", b)
	}
}

// TestWriteFileSaysTheOperationThatFailed pins that a failure other than a permission
// error says the operation and its error, and leaves no new file.
func TestWriteFileSaysTheOperationThatFailed(t *testing.T) {
	dir := stateDir(t)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
	// A folder at the file's path makes the rename over it fail.
	if err := os.Mkdir(filepath.Join(dir, EnrolmentFile), 0o700); err != nil {
		t.Fatal(err)
	}
	err := writeFile(dir, EnrolmentFile, []byte("{}"))
	if err == nil || !strings.HasPrefix(err.Error(), "Fix the state folder "+dir+", then start the provisioner again: renaming a new file to enrolment.json failed: rename ") {
		t.Errorf("%v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%d entries", len(entries))
	}
}

// TestRemoveTemporaryRemovesTheNewFilesACrashLeft pins that the new files of the state
// files' names are removed, and every other entry is left as it is.
func TestRemoveTemporaryRemovesTheNewFilesACrashLeft(t *testing.T) {
	dir, e, k := saved(t)
	left := []string{".access-key-secret.0123456789abcdef", ".enrolment.json.fedcba9876543210", ".instance-id.00112233445566aa"}
	kept := []string{".access-key-secret.0123", ".other.0123456789abcdef", ".access-key-secret.0123456789abcdeg", ".access-key-secret.0123456789abcdef0", "notes", KeyFile, EnrolmentFile}
	for _, name := range append(left, kept...) {
		if name == KeyFile || name == EnrolmentFile {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(k.Secret()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	folder := filepath.Join(dir, ".instance-id.ffffffffffffffff")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemporary(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range left {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left: %v", name, err)
		}
	}
	for _, name := range append(kept, filepath.Base(folder)) {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
	if _, err := Load(dir, e.ApiaryURL); err != nil {
		t.Errorf("%v", err)
	}
}
