package pipeline

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"math/big"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	"gopkg.in/guregu/null.v4"

	"github.com/smartcontractkit/chainlink/core/chains/evm"
	"github.com/smartcontractkit/chainlink/core/chains/evm/config"
	"github.com/smartcontractkit/chainlink/core/logger"
	cnull "github.com/smartcontractkit/chainlink/core/null"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/utils"
)

const (
	CronJobType               string = "cron"
	DirectRequestJobType      string = "directrequest"
	FluxMonitorJobType        string = "fluxmonitor"
	OffchainReportingJobType  string = "offchainreporting"
	OffchainReporting2JobType string = "offchainreporting2"
	KeeperJobType             string = "keeper"
	VRFJobType                string = "vrf"
	BlockhashStoreJobType     string = "blockhashstore"
	WebhookJobType            string = "webhook"
	BootstrapJobType          string = "bootstrap"
)

//go:generate mockery --name Config --output ./mocks/ --case=underscore
//go:generate mockery --name Task --output ./mocks/ --case=underscore

type (
	Task interface {
		Type() TaskType
		ID() int
		DotID() string
		Run(ctx context.Context, lggr logger.Logger, vars Vars, inputs []Result) (Result, RunInfo)
		Base() *BaseTask
		Outputs() []Task
		Inputs() []TaskDependency
		OutputIndex() int32
		TaskTimeout() (time.Duration, bool)
		TaskRetries() uint32
		TaskMinBackoff() time.Duration
		TaskMaxBackoff() time.Duration
	}

	Config interface {
		BridgeResponseURL() *url.URL
		DatabaseURL() url.URL
		DefaultHTTPLimit() int64
		DefaultHTTPTimeout() models.Duration
		TriggerFallbackDBPollInterval() time.Duration
		JobPipelineMaxRunDuration() time.Duration
		JobPipelineReaperInterval() time.Duration
		JobPipelineReaperThreshold() time.Duration
	}
)

// Wraps the input Task for the given dependent task along with a bool variable PropagateResult,
// which Indicates whether result of InputTask should be propagated to its dependent task.
// If the edge between these tasks was an implicit edge, then results are not propagated. This is because
// some tasks cannot handle an input from an edge which wasn't specified in the spec.
type TaskDependency struct {
	PropagateResult bool
	InputTask       Task
}

var (
	ErrWrongInputCardinality = errors.New("wrong number of task inputs")
	ErrBadInput              = errors.New("bad input for task")
	ErrInputTaskErrored      = errors.New("input task errored")
	ErrParameterEmpty        = errors.New("parameter is empty")
	ErrIndexOutOfRange       = errors.New("index out of range")
	ErrTooManyErrors         = errors.New("too many errors")
	ErrTimeout               = errors.New("timeout")
	ErrTaskRunFailed         = errors.New("task run failed")
	ErrCancelled             = errors.New("task run cancelled (fail early)")
)

const (
	InputTaskKey = "input"
)

// RunInfo contains additional information about the finished TaskRun
type RunInfo struct {
	IsRetryable bool
	IsPending   bool
}

// retryableMeta should be returned if the error is non-deterministic; i.e. a
// repeated attempt sometime later _might_ succeed where the current attempt
// failed
func retryableRunInfo() RunInfo {
	return RunInfo{IsRetryable: true}
}

func pendingRunInfo() RunInfo {
	return RunInfo{IsPending: true}
}

func isRetryableHTTPError(statusCode int, err error) bool {
	if statusCode >= 400 && statusCode < 500 {
		// Client errors are not likely to succeed by resubmitting the exact same information again
		return false
	} else if statusCode >= 500 {
		// Remote errors _might_ work on a retry
		return true
	}
	return err != nil
}

// Result is the result of a TaskRun
type Result struct {
	Value interface{}
	Error error
}

// OutputDB dumps a single result output for a pipeline_run or pipeline_task_run
func (result Result) OutputDB() JSONSerializable {
	return JSONSerializable{Val: result.Value, Valid: !(result.Value == nil || (reflect.ValueOf(result.Value).Kind() == reflect.Ptr && reflect.ValueOf(result.Value).IsNil()))}
}

// ErrorDB dumps a single result error for a pipeline_task_run
func (result Result) ErrorDB() null.String {
	var errString null.String
	if result.Error != nil {
		errString = null.StringFrom(result.Error.Error())
	}
	return errString
}

// FinalResult is the result of a Run
type FinalResult struct {
	Values      []interface{}
	AllErrors   []error
	FatalErrors []error
}

// HasFatalErrors returns true if the final result has any errors
func (result FinalResult) HasFatalErrors() bool {
	for _, err := range result.FatalErrors {
		if err != nil {
			return true
		}
	}
	return false
}

// HasErrors returns true if the final result has any errors
func (result FinalResult) HasErrors() bool {
	for _, err := range result.AllErrors {
		if err != nil {
			return true
		}
	}
	return false
}

// SingularResult returns a single result if the FinalResult only has one set of outputs/errors
func (result FinalResult) SingularResult() (Result, error) {
	if len(result.FatalErrors) != 1 || len(result.Values) != 1 {
		return Result{}, errors.Errorf("cannot cast FinalResult to singular result; it does not have exactly 1 error and exactly 1 output: %#v", result)
	}
	return Result{Error: result.FatalErrors[0], Value: result.Values[0]}, nil
}

// TaskRunResult describes the result of a task run, suitable for database
// update or insert.
// ID might be zero if the TaskRun has not been inserted yet
// TaskSpecID will always be non-zero
type TaskRunResult struct {
	ID         uuid.UUID
	Task       Task
	TaskRun    TaskRun
	Result     Result
	Attempts   uint
	CreatedAt  time.Time
	FinishedAt null.Time
	// runInfo is never persisted
	runInfo RunInfo
}

func (result *TaskRunResult) IsPending() bool {
	return !result.FinishedAt.Valid && result.Result == Result{}
}

func (result *TaskRunResult) IsTerminal() bool {
	return len(result.Task.Outputs()) == 0
}

// TaskRunResults represents a collection of results for all task runs for one pipeline run
type TaskRunResults []TaskRunResult

// FinalResult pulls the FinalResult for the pipeline_run from the task runs
// It needs to respect the output index of each task
func (trrs TaskRunResults) FinalResult(l logger.Logger) FinalResult {
	var found bool
	var fr FinalResult
	sort.Slice(trrs, func(i, j int) bool {
		return trrs[i].Task.OutputIndex() < trrs[j].Task.OutputIndex()
	})
	for _, trr := range trrs {
		fr.AllErrors = append(fr.AllErrors, trr.Result.Error)
		if trr.IsTerminal() {
			fr.Values = append(fr.Values, trr.Result.Value)
			fr.FatalErrors = append(fr.FatalErrors, trr.Result.Error)
			found = true
		}
	}

	if !found {
		l.Panicw("Expected at least one task to be final", "tasks", trrs)
	}
	return fr
}

type JSONSerializable struct {
	Val   interface{}
	Valid bool
}

// UnmarshalJSON implements custom unmarshaling logic
func (js *JSONSerializable) UnmarshalJSON(bs []byte) error {
	if js == nil {
		*js = JSONSerializable{}
	}
	if len(bs) == 0 {
		js.Valid = false
		return nil
	}
	err := json.Unmarshal(bs, &js.Val)
	js.Valid = err == nil && js.Val != nil
	return err
}

// MarshalJSON implements custom marshaling logic
func (js JSONSerializable) MarshalJSON() ([]byte, error) {
	if !js.Valid {
		return json.Marshal(nil)
	}
	jsWithHex := replaceBytesWithHex(js.Val)
	return json.Marshal(jsWithHex)
}

func (js *JSONSerializable) Scan(value interface{}) error {
	if value == nil {
		*js = JSONSerializable{}
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return errors.Errorf("JSONSerializable#Scan received a value of type %T", value)
	}
	if js == nil {
		*js = JSONSerializable{}
	}
	return js.UnmarshalJSON(bytes)
}

func (js JSONSerializable) Value() (driver.Value, error) {
	if !js.Valid {
		return nil, nil
	}
	return js.MarshalJSON()
}

func (js *JSONSerializable) Empty() bool {
	return js == nil || !js.Valid
}

type TaskType string

func (t TaskType) String() string {
	return string(t)
}

const (
	TaskTypeHTTP             TaskType = "http"
	TaskTypeBridge           TaskType = "bridge"
	TaskTypeMean             TaskType = "mean"
	TaskTypeMedian           TaskType = "median"
	TaskTypeMode             TaskType = "mode"
	TaskTypeSum              TaskType = "sum"
	TaskTypeMultiply         TaskType = "multiply"
	TaskTypeDivide           TaskType = "divide"
	TaskTypeJSONParse        TaskType = "jsonparse"
	TaskTypeCBORParse        TaskType = "cborparse"
	TaskTypeAny              TaskType = "any"
	TaskTypeVRF              TaskType = "vrf"
	TaskTypeVRFV2            TaskType = "vrfv2"
	TaskTypeEstimateGasLimit TaskType = "estimategaslimit"
	TaskTypeETHCall          TaskType = "ethcall"
	TaskTypeETHTx            TaskType = "ethtx"
	TaskTypeETHABIEncode     TaskType = "ethabiencode"
	TaskTypeETHABIEncode2    TaskType = "ethabiencode2"
	TaskTypeETHABIDecode     TaskType = "ethabidecode"
	TaskTypeETHABIDecodeLog  TaskType = "ethabidecodelog"
	TaskTypeMerge            TaskType = "merge"
	TaskTypeLowercase        TaskType = "lowercase"
	TaskTypeUppercase        TaskType = "uppercase"
	TaskTypeConditional      TaskType = "conditional"
	TaskTypeHexDecode        TaskType = "hexdecode"
	TaskTypeBase64Decode     TaskType = "base64decode"

	// Testing only.
	TaskTypePanic TaskType = "panic"
	TaskTypeMemo  TaskType = "memo"
	TaskTypeFail  TaskType = "fail"
)

var (
	stringType     = reflect.TypeOf("")
	bytesType      = reflect.TypeOf([]byte(nil))
	bytes20Type    = reflect.TypeOf([20]byte{})
	int32Type      = reflect.TypeOf(int32(0))
	nullUint32Type = reflect.TypeOf(cnull.Uint32{})
)

func UnmarshalTaskFromMap(taskType TaskType, taskMap interface{}, ID int, dotID string) (_ Task, err error) {
	defer utils.WrapIfError(&err, "UnmarshalTaskFromMap")

	switch taskMap.(type) {
	default:
		return nil, errors.Errorf("UnmarshalTaskFromMap only accepts a map[string]interface{} or a map[string]string. Got %v (%#v) of type %T", taskMap, taskMap, taskMap)
	case map[string]interface{}, map[string]string:
	}

	taskType = TaskType(strings.ToLower(string(taskType)))

	var task Task
	switch taskType {
	case TaskTypePanic:
		task = &PanicTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeHTTP:
		task = &HTTPTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeBridge:
		task = &BridgeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMean:
		task = &MeanTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMedian:
		task = &MedianTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMode:
		task = &ModeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeSum:
		task = &SumTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeAny:
		task = &AnyTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeJSONParse:
		task = &JSONParseTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMemo:
		task = &MemoTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMultiply:
		task = &MultiplyTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeDivide:
		task = &DivideTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeVRF:
		task = &VRFTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeVRFV2:
		task = &VRFTaskV2{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeEstimateGasLimit:
		task = &EstimateGasLimitTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHCall:
		task = &ETHCallTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHTx:
		task = &ETHTxTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHABIEncode:
		task = &ETHABIEncodeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHABIEncode2:
		task = &ETHABIEncodeTask2{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHABIDecode:
		task = &ETHABIDecodeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeETHABIDecodeLog:
		task = &ETHABIDecodeLogTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeCBORParse:
		task = &CBORParseTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeFail:
		task = &FailTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeMerge:
		task = &MergeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeLowercase:
		task = &LowercaseTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeUppercase:
		task = &UppercaseTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeConditional:
		task = &ConditionalTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeHexDecode:
		task = &HexDecodeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	case TaskTypeBase64Decode:
		task = &Base64DecodeTask{BaseTask: BaseTask{id: ID, dotID: dotID}}
	default:
		return nil, errors.Errorf(`unknown task type: "%v"`, taskType)
	}

	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           task,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			func(from reflect.Type, to reflect.Type, data interface{}) (interface{}, error) {
				if from != stringType {
					return data, nil
				}
				switch to {
				case nullUint32Type:
					i, err2 := strconv.ParseUint(data.(string), 10, 32)
					return cnull.Uint32From(uint32(i)), err2
				}
				return data, nil
			},
		),
	})
	if err != nil {
		return nil, err
	}

	err = decoder.Decode(taskMap)
	if err != nil {
		return nil, err
	}
	return task, nil
}

func CheckInputs(inputs []Result, minLen, maxLen, maxErrors int) ([]interface{}, error) {
	if minLen >= 0 && len(inputs) < minLen {
		return nil, errors.Wrapf(ErrWrongInputCardinality, "min: %v max: %v (got %v)", minLen, maxLen, len(inputs))
	} else if maxLen >= 0 && len(inputs) > maxLen {
		return nil, errors.Wrapf(ErrWrongInputCardinality, "min: %v max: %v (got %v)", minLen, maxLen, len(inputs))
	}
	var vals []interface{}
	var errs int
	for _, input := range inputs {
		if input.Error != nil {
			errs++
			continue
		}
		vals = append(vals, input.Value)
	}
	if maxErrors >= 0 && errs > maxErrors {
		return nil, ErrTooManyErrors
	}
	return vals, nil
}

func getChainByString(chainSet evm.ChainSet, str string) (evm.Chain, error) {
	if str == "" {
		return chainSet.Default()
	}
	id, ok := new(big.Int).SetString(str, 10)
	if !ok {
		return nil, errors.Errorf("invalid EVM chain ID: %s", str)
	}
	return chainSet.Get(id)
}

func SelectGasLimit(cfg config.ChainScopedConfig, jobType string, specGasLimit *uint32) uint64 {
	if specGasLimit != nil {
		return uint64(*specGasLimit)
	}

	var jobTypeGasLimit *uint64
	switch jobType {
	case DirectRequestJobType:
		jobTypeGasLimit = cfg.EvmGasLimitDRJobType()
	case FluxMonitorJobType:
		jobTypeGasLimit = cfg.EvmGasLimitFMJobType()
	case OffchainReportingJobType:
		jobTypeGasLimit = cfg.EvmGasLimitOCRJobType()
	case KeeperJobType:
		jobTypeGasLimit = cfg.EvmGasLimitKeeperJobType()
	case VRFJobType:
		jobTypeGasLimit = cfg.EvmGasLimitVRFJobType()
	}

	if jobTypeGasLimit != nil {
		return *jobTypeGasLimit
	}
	return cfg.EvmGasLimitDefault()
}

// replaceBytesWithHex replaces all []byte with hex-encoded strings
func replaceBytesWithHex(val interface{}) interface{} {
	switch value := val.(type) {
	case nil:
		return value
	case []byte:
		return utils.StringToHex(string(value))
	case common.Address:
		return value.Hex()
	case common.Hash:
		return value.Hex()
	case [][]byte:
		var list []string
		for _, bytes := range value {
			list = append(list, utils.StringToHex(string(bytes)))
		}
		return list
	case []common.Address:
		var list []string
		for _, addr := range value {
			list = append(list, addr.Hex())
		}
		return list
	case []common.Hash:
		var list []string
		for _, hash := range value {
			list = append(list, hash.Hex())
		}
		return list
	case []interface{}:
		if value == nil {
			return value
		}
		var list []interface{}
		for _, item := range value {
			list = append(list, replaceBytesWithHex(item))
		}
		return list
	case map[string]interface{}:
		if value == nil {
			return value
		}
		m := make(map[string]interface{})
		for k, v := range value {
			m[k] = replaceBytesWithHex(v)
		}
		return m
	default:
		// This handles solidity types: bytes1..bytes32,
		// which map to [1]uint8..[32]uint8 when decoded.
		// We persist them as hex strings, and we know ETH ABI encoders
		// can parse hex strings, same as BytesParam does.
		if s := uint8ArrayToSlice(value); s != nil {
			return replaceBytesWithHex(s)
		}
		return value
	}
}

// uint8ArrayToSlice converts [N]uint8 array to slice.
func uint8ArrayToSlice(arr interface{}) interface{} {
	t := reflect.TypeOf(arr)
	if t.Kind() != reflect.Array || t.Elem().Kind() != reflect.Uint8 {
		return nil
	}
	v := reflect.ValueOf(arr)
	s := reflect.MakeSlice(reflect.SliceOf(t.Elem()), v.Len(), v.Len())
	reflect.Copy(s, v)
	return s.Interface()
}
