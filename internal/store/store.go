package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ai-club/sandbox-host/internal/apimodel"
	bolt "go.etcd.io/bbolt"
)

var (
	bSandboxes  = []byte("sandboxes")
	bSessions   = []byte("sessions")
	bSnapshots  = []byte("snapshots")
	ErrNotFound = errors.New("not found")
)

type Store struct{ db *bolt.DB }

func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bSandboxes, bSessions, bSnapshots} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func sandboxKey(owner, project, name string) []byte {
	return []byte(owner + "\x00" + project + "\x00" + name)
}

func (s *Store) PutSandbox(record apimodel.SandboxRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		key := sandboxKey(record.OwnerID, record.ProjectID, record.Sandbox.Name)
		if previous := tx.Bucket(bSandboxes).Get(key); previous != nil {
			var old apimodel.SandboxRecord
			if err := json.Unmarshal(previous, &old); err != nil {
				return err
			}
			if old.Session.ID != record.Session.ID {
				if err := tx.Bucket(bSessions).Delete([]byte(old.Session.ID)); err != nil {
					return err
				}
			}
		}
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bSandboxes).Put(key, b); err != nil {
			return err
		}
		return tx.Bucket(bSessions).Put([]byte(record.Session.ID), key)
	})
}

func (s *Store) GetSandbox(owner, project, name string) (apimodel.SandboxRecord, error) {
	var result apimodel.SandboxRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bSandboxes).Get(sandboxKey(owner, project, name))
		if b == nil {
			return ErrNotFound
		}
		return json.Unmarshal(b, &result)
	})
	return result, err
}
func (s *Store) GetBySession(id string) (apimodel.SandboxRecord, error) {
	var result apimodel.SandboxRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		key := tx.Bucket(bSessions).Get([]byte(id))
		if key == nil {
			return ErrNotFound
		}
		b := tx.Bucket(bSandboxes).Get(key)
		if b == nil {
			return ErrNotFound
		}
		return json.Unmarshal(b, &result)
	})
	return result, err
}

func (s *Store) DeleteSandbox(owner, project, name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		key := sandboxKey(owner, project, name)
		b := tx.Bucket(bSandboxes).Get(key)
		if b == nil {
			return ErrNotFound
		}
		var record apimodel.SandboxRecord
		if err := json.Unmarshal(b, &record); err != nil {
			return err
		}
		if err := tx.Bucket(bSessions).Delete([]byte(record.Session.ID)); err != nil {
			return err
		}
		return tx.Bucket(bSandboxes).Delete(key)
	})
}

func (s *Store) ListSandboxes(owner, project string) ([]apimodel.SandboxRecord, error) {
	var out []apimodel.SandboxRecord
	prefix := []byte(owner + "\x00" + project + "\x00")
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bSandboxes).Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var record apimodel.SandboxRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			out = append(out, record)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Sandbox.CreatedAt > out[j].Sandbox.CreatedAt })
	return out, err
}

func (s *Store) PutSnapshot(record apimodel.SnapshotRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return tx.Bucket(bSnapshots).Put([]byte(record.Snapshot.ID), b)
	})
}
func (s *Store) GetSnapshot(id string) (apimodel.SnapshotRecord, error) {
	var result apimodel.SnapshotRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bSnapshots).Get([]byte(id))
		if b == nil {
			return ErrNotFound
		}
		return json.Unmarshal(b, &result)
	})
	return result, err
}
func (s *Store) DeleteSnapshot(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket(bSnapshots).Get([]byte(id)) == nil {
			return ErrNotFound
		}
		return tx.Bucket(bSnapshots).Delete([]byte(id))
	})
}
func (s *Store) ListSnapshots(owner, project, name string) ([]apimodel.SnapshotRecord, error) {
	var out []apimodel.SnapshotRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bSnapshots).ForEach(func(_, v []byte) error {
			var record apimodel.SnapshotRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return fmt.Errorf("decode snapshot: %w", err)
			}
			if record.OwnerID == owner && record.ProjectID == project && (name == "" || record.Name == name) {
				out = append(out, record)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Snapshot.CreatedAt > out[j].Snapshot.CreatedAt })
	return out, err
}
