package proto

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func encodeConversationTokenDetails(used, max uint64) []byte {
	var inner []byte
	inner = protowire.AppendTag(inner, CTD_UsedTokens, protowire.VarintType)
	inner = protowire.AppendVarint(inner, used)
	inner = protowire.AppendTag(inner, CTD_MaxTokens, protowire.VarintType)
	inner = protowire.AppendVarint(inner, max)

	var checkpoint []byte
	checkpoint = protowire.AppendTag(checkpoint, CSS_Turns, protowire.BytesType)
	checkpoint = protowire.AppendBytes(checkpoint, []byte{0x01})
	checkpoint = protowire.AppendTag(checkpoint, CSS_TokenDetails, protowire.BytesType)
	checkpoint = protowire.AppendBytes(checkpoint, inner)
	return checkpoint
}

func TestDecodeConversationTokenDetails(t *testing.T) {
	got := DecodeConversationTokenDetails(encodeConversationTokenDetails(4821, 200000))
	if got.UsedTokens != 4821 || got.MaxTokens != 200000 {
		t.Fatalf("token details = %+v, want used=4821 max=200000", got)
	}
	if DecodeConversationUsedTokens(encodeConversationTokenDetails(99, 1)) != 99 {
		t.Fatal("used_tokens helper missed field 1")
	}
	if DecodeConversationUsedTokens(nil) != 0 {
		t.Fatal("empty checkpoint should report 0 used_tokens")
	}
	if DecodeConversationUsedTokens([]byte{0x12, 0x01, 0xff}) != 0 {
		t.Fatal("checkpoint without token_details should report 0")
	}
}
