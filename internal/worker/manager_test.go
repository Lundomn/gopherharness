package worker

import (
	"context"
	"testing"
	"time"
)

func TestWorkerCompletes(t *testing.T) {
	m := New(func(_ context.Context, role, prompt string, _ <-chan string) (string, error) {
		return role + ":" + prompt, nil
	})
	id, err := m.Start("explore", "inspect repo")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, w := range m.List() {
			if w.ID == id && w.Status == "completed" {
				if w.Result != "explore:inspect repo" {
					t.Fatal(w.Result)
				}
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not complete")
}

func TestWorkerReceivesMessage(t *testing.T) {
	received := make(chan string, 1)
	m := New(func(ctx context.Context, _, _ string, inbox <-chan string) (string, error) {
		select {
		case message := <-inbox:
			received <- message
			return message, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	id, err := m.Start("explore", "inspect repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(id, "also inspect tests"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "also inspect tests" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive message")
	}
}

func TestCloseCancelsWorkersAndRejectsNewWork(t *testing.T) {
	started := make(chan struct{})
	m := New(func(ctx context.Context, _, _ string, _ <-chan string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	id, err := m.Start("explore", "wait")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m.Close()
	if err = m.Send(id, "late"); err == nil {
		t.Fatal("message accepted after close")
	}
	if _, err = m.Start("explore", "late"); err == nil {
		t.Fatal("worker started after close")
	}
	if workers := m.List(); len(workers) != 1 || workers[0].Status != "stopped" {
		t.Fatalf("workers=%#v", workers)
	}
}
