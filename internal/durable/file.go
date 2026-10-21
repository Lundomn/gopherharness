// Package durable supplies locked read/modify/replace transactions. Network and
// model calls must happen outside Update; it is a local state boundary only.
package durable

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

var locks sync.Map

type File[T any] struct{ Path string }

func (f File[T]) transaction(ctx context.Context, fn func(*T) error, write bool) error {
	path, err := filepath.Abs(f.Path)
	if err != nil {
		return err
	}
	semaphore, _ := locks.LoadOrStore(path, make(chan struct{}, 1))
	gate := semaphore.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := acquire(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer lock()
	var value T
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &value); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = fn(&value); err != nil {
		return err
	}
	if !write {
		return nil
	}
	data, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Replace(path, append(data, '\n'))
}

// Lease holds the operating-system file lock until release or process death.
func Lease(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	return acquire(ctx, path)
}
func (f File[T]) Read(ctx context.Context) (value T, err error) {
	err = f.transaction(ctx, func(v *T) error { value = *v; return nil }, false)
	return
}
func (f File[T]) Update(ctx context.Context, fn func(*T) error) error {
	return f.transaction(ctx, fn, true)
}
func Replace(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
