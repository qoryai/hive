package enrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qoryai/forager/accesskey"
)

// codeFileInterval is how often Start reads a missing enrolment code file again: the
// interval at which Qory Apiary writes the file while the node pool has no access key.
const codeFileInterval = 10 * time.Second

// maxCodeFile is the most bytes the enrolment code file may contain: the longest code,
// a prefix, 26 characters and two fingerprints of 22 characters each after a dot, and
// one newline.
const maxCodeFile = len(accesskey.CodePrefixPool) + 26 + 2*(1+22) + 1

// errMissing is the error of an enrolment code file that is not there yet.
var errMissing = errors.New("the enrolment code file is missing")

// waitCodeFile returns the content of the enrolment code file at path, as readCodeFile
// reads it. While the file is missing, it logs one line and reads the file again every
// codeFileInterval, until the file is there or ctx ends.
func waitCodeFile(ctx context.Context, c Config, path string) (string, error) {
	euid := uint32(os.Geteuid())
	logged := false
	for {
		raw, err := readCodeFile(path, euid, systemNames)
		if !errors.Is(err, errMissing) {
			return raw, err
		}
		if !logged {
			c.Log.Info("the enrolment code file is missing: the provisioner reads it again every 10 seconds until Qory Apiary writes it", slog.String("path", path))
			logged = true
		}
		if err := c.Sleep(ctx, codeFileInterval); err != nil {
			return "", fmt.Errorf("Start the provisioner again: it stopped before Qory Apiary wrote the enrolment code file %s: %w", path, err)
		}
	}
}

// sleep returns after d, or with ctx's error once ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readCodeFile returns the content of the enrolment code file at path, without one
// trailing newline, or errMissing when the file or a folder above it is not there. Before
// it reads a byte, it checks the file and every folder above it, on the path as given
// and on the path with its links resolved, by [trusted]: so only root and the user euid
// may change the code. The file is a regular file of at most maxCodeFile bytes. No error
// contains the content.
func readCodeFile(path string, euid uint32, n names) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errMissing
	}
	if err != nil {
		return "", readError(path, err)
	}
	// O_NOFOLLOW refuses a link put in place after EvalSymlinks, and O_NONBLOCK keeps
	// the open of a named pipe from blocking.
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errMissing
	}
	if err != nil {
		return "", readError(path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", readError(path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Set enrolment_code_file in the settings file to a regular file: %s is not one (%s).", resolved, info.Mode())
	}
	if err := trusted(resolved, "", ownerOf(info), euid, n); err != nil {
		return "", err
	}
	if err := chainTrusted(filepath.Dir(resolved), euid, n); err != nil {
		return "", err
	}
	if resolved != path {
		if err := chainTrusted(path, euid, n); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(maxCodeFile)+1))
	if err != nil {
		return "", readError(path, err)
	}
	if len(b) > maxCodeFile {
		return "", fmt.Errorf("Set enrolment_code_file in the settings file to the file in which Qory Apiary writes a node pool's enrolment code: %s is longer than one code and a newline, %d bytes.", resolved, maxCodeFile)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// readError is the error of reading the enrolment code file at path that failed with
// err: a permission error is to be fixed for the uid the provisioner runs as, and any
// other error is given as it is.
func readError(path string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("Make the enrolment code file %s and its folders readable by uid %d, then start the provisioner again: %w", path, os.Geteuid(), err)
	}
	return fmt.Errorf("Fix the enrolment code file %s, then start the provisioner again: reading it failed: %w", path, err)
}

// fileOwner is what [trusted] reads of a file: its owner, its group and its mode.
type fileOwner struct {
	UID, GID uint32
	Mode     os.FileMode
}

// ownerOf is the owner, the group and the mode of info.
func ownerOf(info fs.FileInfo) fileOwner {
	o := fileOwner{Mode: info.Mode()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		o.UID, o.GID = st.Uid, st.Gid
	}
	return o
}

// names turns an owner and a group into the names [trusted] compares, "" for one the
// system has no name for, and returns a user's primary group.
type names struct {
	User    func(uid uint32) string
	Group   func(gid uint32) string
	Primary func(uid uint32) (gid uint32, ok bool)
}

// systemNames are the names from the system's user and group databases.
var systemNames = names{
	User: func(uid uint32) string {
		if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
			return u.Username
		}
		return ""
	},
	Group: func(gid uint32) string {
		if g, err := user.LookupGroupId(strconv.FormatUint(uint64(gid), 10)); err == nil {
			return g.Name
		}
		return ""
	},
	Primary: func(uid uint32) (uint32, bool) {
		u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
		if err != nil {
			return 0, false
		}
		gid, err := strconv.ParseUint(u.Gid, 10, 32)
		return uint32(gid), err == nil
	},
}

// trusted is the rule the enrolment code file and every folder above it keep, the rule
// that qory keeps for a program it runs: the owner is root or the user euid. A folder
// that root owns with the sticky bit set, such as /tmp, keeps the rule. Other users
// may write nothing else. The group may write it only when the group is gid 0, wheel,
// admin, or the owner's primary group with the owner's name. what is "the link " for a
// link, whose mode no system uses, so only its owner is checked.
func trusted(path, what string, o fileOwner, euid uint32, n names) error {
	if o.UID != 0 && o.UID != euid {
		return fmt.Errorf("Make root or uid %d the owner of %s%s, then start the provisioner again: uid %d owns it, so a user other than root and the provisioner could change the enrolment code.", euid, what, path, o.UID)
	}
	if what != "" || o.Mode.IsDir() && o.UID == 0 && o.Mode&os.ModeSticky != 0 {
		return nil
	}
	perm := o.Mode.Perm()
	if perm&0o002 != 0 {
		return fmt.Errorf("Remove the write permission of other users from %s, then start the provisioner again: every user may write it (%s, owner uid %d).", path, o.Mode, o.UID)
	}
	if perm&0o020 != 0 {
		group := n.Group(o.GID)
		primary, ok := n.Primary(o.UID)
		owners := group != "" && group == n.User(o.UID) && ok && primary == o.GID
		if o.GID != 0 && group != "wheel" && group != "admin" && !owners {
			return fmt.Errorf("Remove the group's write permission from %s, then start the provisioner again: the group %s may write it (%s, owner uid %d). A group may write it only when it is gid 0, wheel, admin, or the owner's primary group with the owner's name.", path, named(group, o.GID), o.Mode, o.UID)
		}
	}
	return nil
}

// named is a group's name with its id beside it, or the id alone.
func named(name string, id uint32) string {
	if name == "" {
		return "gid " + strconv.FormatUint(uint64(id), 10)
	}
	return fmt.Sprintf("%s (gid %d)", name, id)
}

// chainTrusted holds path and every folder above it up to / to [trusted], each read with
// os.Lstat, so a link is checked itself, as a link.
func chainTrusted(path string, euid uint32, n names) error {
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return readError(path, err)
		}
		what := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			what = "the link "
		}
		if err := trusted(p, what, ownerOf(info), euid, n); err != nil {
			return err
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}
