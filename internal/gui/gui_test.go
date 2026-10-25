package gui

import (
	"testing"
	"time"

	"krd-updater/internal/config"
)

func TestTouchMarksPageSeen(t *testing.T) {
	s := New(config.Config{})
	s.mu.Lock()
	seen := s.pageSeen
	s.mu.Unlock()
	if seen {
		t.Fatal("刚创建就认为页面已经打开过")
	}
	s.touch()
	s.mu.Lock()
	seen, last := s.pageSeen, s.lastSeen
	s.mu.Unlock()
	if !seen || time.Since(last) > time.Second {
		t.Fatalf("touch 之后状态不对: seen=%v last=%v", seen, last)
	}
}

func TestScheduleQuitCanBeCancelled(t *testing.T) {
	s := New(config.Config{})
	s.touch()
	s.scheduleQuit(80 * time.Millisecond)
	s.cancelQuit()
	select {
	case <-s.quitCh:
		t.Fatal("取消之后还是退出了")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestScheduleQuitFires(t *testing.T) {
	s := New(config.Config{})
	s.scheduleQuit(40 * time.Millisecond)
	select {
	case <-s.quitCh:
	case <-time.After(time.Second):
		t.Fatal("到点没有退出")
	}
}

func TestRequestQuitIsIdempotent(t *testing.T) {
	s := New(config.Config{})
	s.requestQuit()
	s.requestQuit()
	select {
	case <-s.quitCh:
	default:
		t.Fatal("quitCh 没有关闭")
	}
}
