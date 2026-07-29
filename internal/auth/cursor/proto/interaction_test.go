package proto

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestDecodeInteractionQuery(t *testing.T) {
	query := newMsg("InteractionQuery")
	setUint32(query, "id", 42)
	setMsg(query, "create_plan_request_query", newMsg("CreatePlanRequestQuery"))
	server := newMsg("AgentServerMessage")
	setMsg(server, "interaction_query", query)

	decoded, err := DecodeAgentServerMessage(marshal(server))
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if decoded.Type != ServerMsgInteractionQuery {
		t.Fatalf("type = %v, want interaction query", decoded.Type)
	}
	if decoded.InteractionQueryID != 42 {
		t.Fatalf("query ID = %d, want 42", decoded.InteractionQueryID)
	}
	if decoded.InteractionQueryKind != InteractionQueryCreatePlan {
		t.Fatalf("query kind = %v, want create plan", decoded.InteractionQueryKind)
	}
}

func TestEncodeInteractionResponsePreservesQueryIDAndPolicy(t *testing.T) {
	tests := []struct {
		name          string
		kind          InteractionQueryKind
		responseField string
	}{
		{name: "create plan", kind: InteractionQueryCreatePlan, responseField: "create_plan_request_response"},
		{name: "ask question", kind: InteractionQueryAskQuestion, responseField: "ask_question_interaction_response"},
		{name: "switch mode", kind: InteractionQuerySwitchMode, responseField: "switch_mode_request_response"},
		{name: "web search", kind: InteractionQueryWebSearch, responseField: "web_search_request_response"},
		{name: "exa search", kind: InteractionQueryExaSearch, responseField: "exa_search_request_response"},
		{name: "exa fetch", kind: InteractionQueryExaFetch, responseField: "exa_fetch_request_response"},
		{name: "setup vm", kind: InteractionQuerySetupVM},
		{name: "unknown", kind: InteractionQueryUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMsg("AgentClientMessage")
			if err := proto.Unmarshal(EncodeInteractionResponse(42, tt.kind), client); err != nil {
				t.Fatalf("decode client message: %v", err)
			}
			interactionField := field(client, "interaction_response")
			if !client.Has(interactionField) {
				t.Fatal("interaction_response is absent")
			}
			response := client.Get(interactionField).Message()
			if got := response.Get(response.Descriptor().Fields().ByName("id")).Uint(); got != 42 {
				t.Fatalf("response id = %d, want 42", got)
			}
			if tt.responseField == "" {
				if response.WhichOneof(response.Descriptor().Oneofs().ByName("result")) != nil {
					t.Fatalf("unexpected response result for %s", tt.name)
				}
				return
			}
			if got := response.WhichOneof(response.Descriptor().Oneofs().ByName("result")); got == nil || string(got.Name()) != tt.responseField {
				t.Fatalf("response result = %v, want %s", got, tt.responseField)
			}
		})
	}
}

func TestPreCommitBootstrapErrorClassification(t *testing.T) {
	if !IsPreCommitBootstrapError(&BootstrapError{}) {
		t.Fatal("uncommitted bootstrap error should be retryable")
	}
	if IsPreCommitBootstrapError(&BootstrapError{committed: true}) {
		t.Fatal("committed bootstrap error must not be retryable")
	}
}
