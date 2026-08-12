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

func TestEncodeExecShellSuccess(t *testing.T) {
	client := newMsg("AgentClientMessage")
	encoded := EncodeExecShellSuccess(7, "exec-7", "printf OK", "/tmp", "OK", "", 0)
	if err := proto.Unmarshal(encoded, client); err != nil {
		t.Fatalf("decode client message: %v", err)
	}

	execField := field(client, "exec_client_message")
	if !client.Has(execField) {
		t.Fatal("exec_client_message is absent")
	}
	exec := client.Get(execField).Message()
	if got := exec.Get(exec.Descriptor().Fields().ByName("id")).Uint(); got != 7 {
		t.Fatalf("exec id = %d, want 7", got)
	}
	shellField := exec.Descriptor().Fields().ByName("shell_result")
	if !exec.Has(shellField) {
		t.Fatal("shell_result is absent")
	}
	shellResult := exec.Get(shellField).Message()
	successField := shellResult.Descriptor().Fields().ByName("success")
	if !shellResult.Has(successField) {
		t.Fatal("shell success is absent")
	}
	success := shellResult.Get(successField).Message()
	if got := success.Get(success.Descriptor().Fields().ByName("command")).String(); got != "printf OK" {
		t.Fatalf("command = %q, want printf OK", got)
	}
	if got := success.Get(success.Descriptor().Fields().ByName("working_directory")).String(); got != "/tmp" {
		t.Fatalf("working directory = %q, want /tmp", got)
	}
	if got := success.Get(success.Descriptor().Fields().ByName("stdout")).String(); got != "OK" {
		t.Fatalf("stdout = %q, want OK", got)
	}
	if got := success.Get(success.Descriptor().Fields().ByName("exit_code")).Int(); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestEncodeExecShellStreamEvents(t *testing.T) {
	tests := []struct {
		name      string
		encoded   []byte
		eventName string
	}{
		{name: "start", encoded: EncodeExecShellStreamStart(8, "exec-8"), eventName: "start"},
		{name: "stdout", encoded: EncodeExecShellStreamStdout(8, "exec-8", "OK"), eventName: "stdout"},
		{name: "exit", encoded: EncodeExecShellStreamExit(8, "exec-8", 0, "/tmp"), eventName: "exit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMsg("AgentClientMessage")
			if err := proto.Unmarshal(tt.encoded, client); err != nil {
				t.Fatalf("decode client message: %v", err)
			}
			exec := client.Get(field(client, "exec_client_message")).Message()
			streamField := exec.Descriptor().Fields().ByName("shell_stream")
			if !exec.Has(streamField) {
				t.Fatal("shell_stream is absent")
			}
			stream := exec.Get(streamField).Message()
			got := stream.WhichOneof(stream.Descriptor().Oneofs().ByName("event"))
			if got == nil || string(got.Name()) != tt.eventName {
				t.Fatalf("stream event = %v, want %s", got, tt.eventName)
			}
		})
	}
}

func TestEncodeExecClientStreamClose(t *testing.T) {
	client := newMsg("AgentClientMessage")
	if err := proto.Unmarshal(EncodeExecClientStreamClose(9), client); err != nil {
		t.Fatalf("decode client message: %v", err)
	}
	controlField := field(client, "exec_client_control_message")
	if !client.Has(controlField) {
		t.Fatal("exec_client_control_message is absent")
	}
	control := client.Get(controlField).Message()
	closeField := control.Descriptor().Fields().ByName("stream_close")
	if !control.Has(closeField) {
		t.Fatal("stream_close is absent")
	}
	closeMsg := control.Get(closeField).Message()
	if got := closeMsg.Get(closeMsg.Descriptor().Fields().ByName("id")).Uint(); got != 9 {
		t.Fatalf("stream close id = %d, want 9", got)
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
