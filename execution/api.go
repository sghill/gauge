package execution

import (
	"log"
	"net"

	"fmt"

	"github.com/getgauge/common"
	"github.com/getgauge/gauge/conn"
	"github.com/getgauge/gauge/execution/event"
	"github.com/getgauge/gauge/execution/rerun"
	"github.com/getgauge/gauge/execution/result"
	"github.com/getgauge/gauge/gauge"
	gm "github.com/getgauge/gauge/gauge_messages"
	"github.com/getgauge/gauge/logger"
	"github.com/getgauge/gauge/validation"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"
)

func Start() {
	port, err := conn.GetPortFromEnvironmentVariable(common.APIV2PortEnvVariableName)
	if err != nil {
		logger.APILog.Error("Failed to start execution API Service. %s \n", err.Error())
		return
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{Port: port})
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	gm.RegisterExecutionServer(s, &executionServer{})
	go s.Serve(listener)
}

type executionServer struct {
}

func (e *executionServer) Execute(req *gm.ExecutionRequest, stream gm.Execution_ExecuteServer) error {
	execute(req.Specs, stream)
	return nil
}

func execute(specDirs []string, stream gm.Execution_ExecuteServer) {
	if err := validateFlags(); err != nil {
		stream.Send(getErrorExecutionResponse(err))
		return
	}
	res := validation.ValidateSpecs(specDirs)
	if len(res.Errs) > 0 {
		stream.Send(getErrorExecutionResponse(res.Errs...))
		return
	}
	event.InitRegistry()
	listenExecutionEvents(stream)
	rerun.ListenFailedScenarios()
	ei := newExecutionInfo(res.SpecCollection, res.Runner, nil, res.ErrMap, InParallel, 0)
	e := newExecution(ei)
	e.run()
}

func listenExecutionEvents(stream gm.Execution_ExecuteServer) {
	ch := make(chan event.ExecutionEvent, 0)
	event.Register(ch, event.SuiteStart, event.SpecStart, event.SpecEnd, event.ScenarioStart, event.ScenarioEnd, event.SuiteEnd)
	go func() {
		for {
			e := <-ch
			res := getResponse(e)
			if stream.Send(res) != nil || res.Type == gm.ExecutionResponse_SuiteEnd.Enum() {
				return
			}
		}
	}()
}

func getResponse(e event.ExecutionEvent) *gm.ExecutionResponse {
	switch e.Topic {
	case event.SuiteStart:
		return &gm.ExecutionResponse{Type: gm.ExecutionResponse_SuiteStart.Enum()}
	case event.SpecStart:
		return &gm.ExecutionResponse{
			Type: gm.ExecutionResponse_SpecStart.Enum(),
			ID:   e.ExecutionInfo.CurrentSpec.FileName,
		}
	case event.ScenarioStart:
		return &gm.ExecutionResponse{
			Type: gm.ExecutionResponse_ScenarioStart.Enum(),
			ID:   proto.String(fmt.Sprintf("%s:%d", e.ExecutionInfo.CurrentSpec.GetFileName(), e.Item.(*gauge.Scenario).Heading.LineNo)),
			Result: &gm.Result{
				TableRowNumber: proto.Int64(int64(getDataTableRowNumber(e.Item.(*gauge.Scenario)))),
			},
		}
	case event.ScenarioEnd:
		scn := e.Item.(*gauge.Scenario)
		return &gm.ExecutionResponse{
			Type: gm.ExecutionResponse_ScenarioEnd.Enum(),
			ID:   proto.String(fmt.Sprintf("%s:%d", e.ExecutionInfo.CurrentSpec.GetFileName(), scn.Heading.LineNo)),
			Result: &gm.Result{
				Status:            getStatus(e.Result.(*result.ScenarioResult)),
				ExecutionTime:     proto.Int64(e.Result.ExecTime()),
				Errors:            getErrors(e.Result.(*result.ScenarioResult).ProtoScenario.GetScenarioItems()),
				BeforeHookFailure: getHookFailure(e.Result.GetPreHook()),
				AfterHookFailure:  getHookFailure(e.Result.GetPostHook()),
				TableRowNumber:    proto.Int64(int64(getDataTableRowNumber(scn))),
			},
		}
	case event.SpecEnd:
		return &gm.ExecutionResponse{
			Type: gm.ExecutionResponse_SpecEnd.Enum(),
			ID:   e.ExecutionInfo.CurrentSpec.FileName,
			Result: &gm.Result{
				BeforeHookFailure: getHookFailure(e.Result.GetPreHook()),
				AfterHookFailure:  getHookFailure(e.Result.GetPostHook()),
			},
		}
	case event.SuiteEnd:
		return &gm.ExecutionResponse{
			Type: gm.ExecutionResponse_SuiteEnd.Enum(),
			Result: &gm.Result{
				BeforeHookFailure: getHookFailure(e.Result.GetPreHook()),
				AfterHookFailure:  getHookFailure(e.Result.GetPostHook()),
			},
		}
	}
	return nil
}

func getDataTableRowNumber(scn *gauge.Scenario) int {
	index := scn.DataTableRowIndex
	if scn.DataTableRow.IsInitialized() {
		index++
	}
	return index
}

func getErrorExecutionResponse(errs ...error) *gm.ExecutionResponse {
	var e []*gm.Result_ExecutionError
	for _, err := range errs {
		e = append(e, &gm.Result_ExecutionError{ErrorMessage: proto.String(err.Error())})
	}
	return &gm.ExecutionResponse{
		Type: gm.ExecutionResponse_ErrorResult.Enum(),
		Result: &gm.Result{
			Errors: e,
		},
	}
}

func getHookFailure(hookFailure **gm.ProtoHookFailure) *gm.Result_ExecutionError {
	if hookFailure != nil && *hookFailure != nil {
		return &gm.Result_ExecutionError{
			Screenshot:   (**hookFailure).ScreenShot,
			ErrorMessage: (**hookFailure).ErrorMessage,
			StackTrace:   (**hookFailure).StackTrace,
		}
	}
	return nil
}

func getErrors(items []*gm.ProtoItem) []*gm.Result_ExecutionError {
	var errors []*gm.Result_ExecutionError
	for _, item := range items {
		executionResult := item.GetStep().GetStepExecutionResult()
		res := executionResult.GetExecutionResult()
		switch item.GetItemType() {
		case gm.ProtoItem_Step:
			if executionResult.GetSkipped() {
				errors = append(errors, &gm.Result_ExecutionError{ErrorMessage: executionResult.SkippedReason})
			} else if res.GetFailed() {
				errors = append(errors, &gm.Result_ExecutionError{
					StackTrace:   res.StackTrace,
					ErrorMessage: res.ErrorMessage,
					Screenshot:   res.ScreenShot,
				})
			}
		case gm.ProtoItem_Concept:
			errors = append(errors, getErrors(item.GetConcept().GetSteps())...)
		}
	}
	return errors
}

func getStatus(result *result.ScenarioResult) *gm.Result_Status {
	if result.GetFailed() {
		return gm.Result_FAILED.Enum()
	}
	if result.ProtoScenario.GetSkipped() {
		return gm.Result_SKIPPED.Enum()
	}
	return gm.Result_PASSED.Enum()
}
