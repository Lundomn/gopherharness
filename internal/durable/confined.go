package durable

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// ReplaceWithin is the workspace-confined variant for untrusted channel
// attachments. os.Root checks symlinks at the actual filesystem operation, not
// only in a preliminary path string check.
func ReplaceWithin(root, relative string, data []byte) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	dir := filepath.Dir(relative)
	if err := r.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := filepath.Join(dir, ".state-"+hex.EncodeToString(nonce[:]))
	f, err := r.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(temporary)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = r.Rename(temporary, relative); err != nil {
		return err
	}
	d, err := r.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
