package telegram

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestCleanupPrunesExpiredChatsAndKeepsActiveSends(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		sender := &Sender{deliveries: map[string]*bookmarkDelivery{
			"expired": {chats: map[string]*chatDelivery{"111": {lastSuccess: now}}},
			"mixed":   {chats: map[string]*chatDelivery{"111": {lastSuccess: now}, "222": {lastSuccess: now.Add(30 * time.Minute)}}},
			"active":  {refs: 1, chats: map[string]*chatDelivery{"111": {lastSuccess: now}}},
		}}
		time.Sleep(time.Hour)
		sender.cleanup(time.Now())
		if _, ok := sender.deliveries["expired"]; ok {
			t.Fatal("expired bookmark retained")
		}
		if got := len(sender.deliveries["mixed"].chats); got != 1 {
			t.Fatalf("mixed chats = %d, want 1", got)
		}
		if got := len(sender.deliveries["active"].chats); got != 1 {
			t.Fatalf("active chats = %d, want 1", got)
		}
	})
}
