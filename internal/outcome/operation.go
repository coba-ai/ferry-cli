package outcome

// Operation names one route of the contract. The two money operations carry
// the server's own operation_id spelling (`app/models/command.rb:194`) and not
// a CLI invention, because AC74 selects the terminal rule by the *command
// body's* `operation` field and a second spelling would be a second
// vocabulary.
type Operation string

const (
	OpGetMe            Operation = "get_me"
	OpListAPIKeys      Operation = "list_api_keys"
	OpCreateAPIKey     Operation = "create_api_key"
	OpGetAPIKey        Operation = "get_api_key"
	OpRevokeAPIKey     Operation = "revoke_api_key"
	OpListCorridors    Operation = "list_corridors"
	OpGetCorridor      Operation = "get_corridor"
	OpSimulateTransfer Operation = "transfers_simulate"
	OpExecuteTransfer  Operation = "transfers_execute"
	OpGetCommand       Operation = "get_command"
)

// Operations is every operation this CLI performs. `api.Routes` is pinned to
// it and to `openapi.yaml` in both directions (AC25), so an operation added
// here without a route, or a route without an operation, is red.
var Operations = []Operation{
	OpGetMe,
	OpListAPIKeys,
	OpCreateAPIKey,
	OpGetAPIKey,
	OpRevokeAPIKey,
	OpListCorridors,
	OpGetCorridor,
	OpSimulateTransfer,
	OpExecuteTransfer,
	OpGetCommand,
}

// OpClass is the operation class C4 names as the table's first input. There
// are four, and they are the four *classification* behaviours PLAN §5.6
// distinguishes — not the four HTTP verbs and not the ten operations.
//
// `control_revoke` is deliberately absent: §5.6 says revocation is "idempotent,
// retried like a read", and a revoke's refusals classify exactly as a read's.
// It differs from a read in nothing this package decides. `control_create` is
// its own class only because a transport failure after the request was written
// means an unowned credential may exist, which is not what it means for a read.
type OpClass string

const (
	OpMoneySimulate OpClass = "money_simulate"
	OpMoneyExecute  OpClass = "money_execute"
	OpRead          OpClass = "read"
	OpControlCreate OpClass = "control_create"
)

// OpClasses is the vocabulary AC19's coverage test iterates.
var OpClasses = []OpClass{OpMoneySimulate, OpMoneyExecute, OpRead, OpControlCreate}

var opClasses = map[Operation]OpClass{
	OpGetMe:            OpRead,
	OpListAPIKeys:      OpRead,
	OpCreateAPIKey:     OpControlCreate,
	OpGetAPIKey:        OpRead,
	OpRevokeAPIKey:     OpRead,
	OpListCorridors:    OpRead,
	OpGetCorridor:      OpRead,
	OpSimulateTransfer: OpMoneySimulate,
	OpExecuteTransfer:  OpMoneyExecute,
	OpGetCommand:       OpRead,
}

// ClassOf reports the operation class of op. The second return is false for an
// operation this package does not declare, which every caller must treat as an
// answer it cannot classify rather than as a read.
func ClassOf(op Operation) (OpClass, bool) {
	c, ok := opClasses[op]

	return c, ok
}

// IsMoney reports whether a class is one of the two that can move money.
func (c OpClass) IsMoney() bool { return c == OpMoneySimulate || c == OpMoneyExecute }

// CommandStates is the `Command.state` vocabulary, pinned to `openapi.yaml`'s
// enum by AC25. Ordered as the contract orders it.
var CommandStates = []string{
	"reserved",
	"inflight",
	"upstream_unknown",
	"completed",
	"failed_retriable",
	"failed_terminal",
	"needs_operator",
}

// TerminalStates are the two states from which no further transition happens
// (`docs/api/openapi.yaml`, Command.state description).
var TerminalStates = []string{"completed", "failed_terminal"}

// KnownState reports whether state is in the contract's enum. A state outside
// it is an answer this CLI cannot act on, and the caller must stop rather than
// poll: see ClassifyCommand.
func KnownState(state string) bool {
	for _, s := range CommandStates {
		if s == state {
			return true
		}
	}

	return false
}

// IsTerminalState reports whether a command in this state will not move again.
func IsTerminalState(state string) bool {
	for _, s := range TerminalStates {
		if s == state {
			return true
		}
	}

	return false
}
