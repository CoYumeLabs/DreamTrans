package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"time"
)

type savedSession struct {
	User              accountUser
	Access, Refresh   string
	Expires, Deadline time.Time
	Revoked           bool
}

func (s *Server) sessionCipher() cipher.AEAD {
	key := sha256.Sum256([]byte("yuaction/session/v1\x00" + s.cfg.CreatorKey))
	block, _ := aes.NewCipher(key[:])
	aead, _ := cipher.NewGCM(block)
	return aead
}
func (s *Server) encodeSession(a *loginSession) ([]byte, error) {
	data, err := json.Marshal(savedSession{a.user, a.access, a.refresh, a.expires, a.deadline, a.revoked})
	if err != nil {
		return nil, err
	}
	aead := s.sessionCipher()
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, data, []byte(a.id)), nil
}
func (s *Server) decodeSession(data []byte, a *loginSession) error {
	aead := s.sessionCipher()
	if len(data) < aead.NonceSize() {
		return fail(401, "请先登录 Yufolo 账号")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(a.id))
	if err != nil {
		return fail(401, "登录信息无法读取，请重新登录")
	}
	var v savedSession
	if err = json.Unmarshal(plain, &v); err != nil {
		return err
	}
	if a.user.ID != "" && a.user.ID != v.User.ID {
		return fail(401, "登录账号已变化")
	}
	// user is immutable once published to callers, avoiding races with archives.
	if a.user.ID == "" {
		a.user = v.User
	}
	a.access, a.refresh, a.expires, a.deadline, a.revoked = v.Access, v.Refresh, v.Expires, v.Deadline, v.Revoked
	return nil
}
func (s *Server) persistSession(ctx context.Context, a *loginSession) error {
	return s.store.MutateShared(ctx, "auth:"+a.id, func(_ []byte) ([]byte, error) { return s.encodeSession(a) })
}
func (s *Server) loadSession(ctx context.Context, id string) (*loginSession, error) {
	// Do not cache remote credentials: logout and token rotation must be visible
	// to existing streams and background jobs on both deployment colors.
	a := &loginSession{server: s, id: id}
	err := s.store.MutateShared(ctx, "auth:"+id, func(data []byte) ([]byte, error) {
		if err := s.decodeSession(data, a); err != nil {
			return nil, err
		}
		if a.revoked || time.Now().After(a.deadline) {
			return nil, fail(401, "请重新登录 Yufolo")
		}
		return nil, nil
	})
	return a, err
}

// Caller holds a.mu. The transaction covers reading, refreshing and storing the
// rotated refresh token, so two generations cannot consume the same token.
func (a *loginSession) mutate(ctx context.Context, change func() error) error {
	if a.server == nil || a.id == "" {
		return change()
	}
	return a.server.store.MutateShared(ctx, "auth:"+a.id, func(data []byte) ([]byte, error) {
		if err := a.server.decodeSession(data, a); err != nil {
			return nil, err
		}
		if err := change(); err != nil {
			return nil, err
		}
		return a.server.encodeSession(a)
	})
}
