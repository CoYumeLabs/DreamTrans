package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

func (s *Server) claimJob(key string) (func(), error) {
	done, admitted := s.deploy.BeginTask()
	if !admitted {
		return nil, fail(503, "服务正在切换，请稍后重试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := s.store.TryLock(ctx, "job:"+key)
	if err != nil {
		done()
		if errors.Is(err, storage.ErrConflict) {
			return nil, fail(409, "此任务正在处理")
		}
		return nil, err
	}
	for i := 0; i < 4; i++ {
		slot, err := s.store.TryLock(ctx, "ai-slot:"+strconv.Itoa(i))
		if err == nil {
			return func() { release(); slot(); done() }, nil
		}
		if !errors.Is(err, storage.ErrConflict) {
			release()
			done()
			return nil, err
		}
	}
	release()
	done()
	return nil, fail(429, "AI 正忙，请稍后重试")
}
func (s *Server) jobActive(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	active, err := s.store.Locked(ctx, "job:"+key)
	return active || err != nil
}
func (s *Server) hostSession(ctx context.Context, code string) *loginSession {
	_, rec, err := s.read(ctx, code)
	if err != nil || rec.Link.AuthSessionID == "" {
		return nil
	}
	a, err := s.loadSession(ctx, rec.Link.AuthSessionID)
	if err != nil || a.user.ID != rec.Link.OwnerID {
		return nil
	}
	return a
}
