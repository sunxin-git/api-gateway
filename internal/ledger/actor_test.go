package ledger

import (
	"testing"
)

func TestActorValidate(t *testing.T) {
	tests := []struct {
		name    string
		actor   Actor
		wantErr bool
	}{
		{"合法 admin_token", Actor{Type: ActorTypeAdminToken, ID: "tok-1"}, false},
		{"合法 cli", Actor{Type: ActorTypeCLI, ID: "bootstrap"}, false},
		{"合法 system", Actor{Type: ActorTypeSystem, ID: "reconciler"}, false},
		{"合法 task", Actor{Type: ActorTypeTask, ID: "task-uuid"}, false},
		{"非法 type", Actor{Type: ActorType("hacker"), ID: "x"}, true},
		{"空 type", Actor{Type: "", ID: "x"}, true},
		{"空 ID", Actor{Type: ActorTypeCLI, ID: ""}, true},
		{"空白 ID", Actor{Type: ActorTypeCLI, ID: "   "}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.actor.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestActorString(t *testing.T) {
	a := Actor{Type: ActorTypeCLI, ID: "bootstrap"}
	got := a.String()
	want := "cli:bootstrap"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
