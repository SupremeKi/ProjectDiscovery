package network

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/v3/pkg/operators"
	"github.com/projectdiscovery/nuclei/v3/pkg/output"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/contextargs"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/expressions"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/generators"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/helpers/eventcreator"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/helpers/responsehighlighter"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/interactsh"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/protocolstate"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/replacer"
	"github.com/projectdiscovery/nuclei/v3/pkg/protocols/common/utils/vardump"
	protocolutils "github.com/projectdiscovery/nuclei/v3/pkg/protocols/utils"
	templateTypes "github.com/projectdiscovery/nuclei/v3/pkg/templates/types"
	errorutil "github.com/projectdiscovery/utils/errors"
	mapsutil "github.com/projectdiscovery/utils/maps"
	"github.com/projectdiscovery/utils/reader"
	syncutil "github.com/projectdiscovery/utils/sync"
)

var (
	// TODO: make this configurable
	// DefaultReadTimeout is the default read timeout for network requests
	DefaultReadTimeout = time.Duration(5) * time.Second
)

var _ protocols.Request = &Request{}

// Type returns the type of the protocol request
func (request *Request) Type() templateTypes.ProtocolType {
	return templateTypes.NetworkProtocol
}

// getOpenPorts returns all open ports from list of ports provided in template
// if only 1 port is provided, no need to check if port is open or not
func (request *Request) getOpenPorts(target *contextargs.Context) ([]string, error) {
	if len(request.ports) == 1 {
		// no need to check if port is open or not
		return request.ports, nil
	}
	errs := []error{}
	// if more than 1 port is provided, check if port is open or not
	openPorts := make([]string, 0)
	for _, port := range request.ports {
		cloned := target.Clone()
		if err := cloned.UseNetworkPort(port, request.ExcludePorts); err != nil {
			errs = append(errs, err)
			continue
		}
		addr, err := getAddress(cloned.MetaInput.Input)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		conn, err := protocolstate.Dialer.Dial(context.TODO(), "tcp", addr)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_ = conn.Close()
		openPorts = append(openPorts, port)
	}
	if len(openPorts) == 0 {
		return nil, multierr.Combine(errs...)
	}
	return openPorts, nil
}

// ExecuteWithResults executes the protocol requests and returns results instead of writing them.
func (request *Request) ExecuteWithResults(target *contextargs.Context, metadata, previous output.InternalEvent) <-chan protocols.Result {
	results := make(chan protocols.Result)
	onResult := func(events ...*output.InternalWrappedEvent) {
		for _, event := range events {
			results <- protocols.Result{Event: event}
		}
	}

	var errGroup errgroup.Group

	errGroup.Go(func() error {
		visitedAddresses := make(mapsutil.Map[string, struct{}])

		if request.Port == "" {
			// backwords compatibility or for other use cases
			// where port is not provided in template
			events, err := request.executeOnTarget(target, visitedAddresses, metadata, previous)
			onResult(events...)
			if err != nil {
				return err
			}
		}

		// get open ports from list of ports provided in template
		ports, err := request.getOpenPorts(target)
		if len(ports) == 0 {
			return err
		}
		if err != nil {
			// TODO: replace this after scan context is implemented
			gologger.Verbose().Msgf("[%v] got errors while checking open ports: %s\n", request.options.TemplateID, err)
		}

		for _, port := range ports {
			input := target.Clone()
			// use network port updates input with new port requested in template file
			// and it is ignored if input port is not standard http(s) ports like 80,8080,8081 etc
			// idea is to reduce redundant dials to http ports
			if err := input.UseNetworkPort(port, request.ExcludePorts); err != nil {
				gologger.Debug().Msgf("Could not network port from constants: %s\n", err)
			}
			events, err := request.executeOnTarget(input, visitedAddresses, metadata, previous)
			onResult(events...)
			if err != nil {
				return err
			}
		}

		return nil
	})

	go func() {
		defer close(results)
		if err := errGroup.Wait(); err != nil {
			results <- protocols.Result{Error: err}
		}
	}()

	return results
}

func (request *Request) executeOnTarget(input *contextargs.Context, visited mapsutil.Map[string, struct{}], metadata, previous output.InternalEvent) ([]*output.InternalWrappedEvent, error) {
	var address string
	var err error

	if request.SelfContained {
		address = ""
	} else {
		address, err = getAddress(input.MetaInput.Input)
	}
	if err != nil {
		request.options.Output.Request(request.options.TemplatePath, input.MetaInput.Input, request.Type().String(), err)
		request.options.Progress.IncrementFailedRequestsBy(1)
		return nil, errors.Wrap(err, "could not get address from url")
	}
	variables := protocolutils.GenerateVariables(address, false, nil)
	// add template ctx variables to varMap
	if request.options.HasTemplateCtx(input.MetaInput) {
		variables = generators.MergeMaps(variables, request.options.GetTemplateCtx(input.MetaInput).GetAll())
	}
	variablesMap := request.options.Variables.Evaluate(variables)
	variables = generators.MergeMaps(variablesMap, variables, request.options.Constants)

	var events []*output.InternalWrappedEvent

	for _, kv := range request.addresses {
		actualAddress := replacer.Replace(kv.address, variables)

		if visited.Has(actualAddress) && !request.options.Options.DisableClustering {
			continue
		}
		visited.Set(actualAddress, struct{}{})

		evs, err := request.executeAddress(variables, actualAddress, address, input, kv.tls, previous)
		if err != nil {
			outputEvent := request.responseToDSLMap("", "", "", address, "")
			events = append(events, &output.InternalWrappedEvent{InternalEvent: outputEvent})
			gologger.Warning().Msgf("[%v] Could not make network request for (%s) : %s\n", request.options.TemplateID, actualAddress, err)
			continue
		}
		events = append(events, evs...)
	}
	return events, nil
}

// executeAddress executes the request for an address
func (request *Request) executeAddress(variables map[string]interface{}, actualAddress, address string, input *contextargs.Context, shouldUseTLS bool, previous output.InternalEvent) ([]*output.InternalWrappedEvent, error) {
	variables = generators.MergeMaps(variables, map[string]interface{}{"Hostname": address})
	payloads := generators.BuildPayloadFromOptions(request.options.Options)

	if !strings.Contains(actualAddress, ":") {
		err := errors.New("no port provided in network protocol request")
		request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
		request.options.Progress.IncrementFailedRequestsBy(1)
		return nil, err
	}

	// if request threads matches global payload concurrency we follow it
	shouldFollowGlobal := request.Threads == request.options.Options.PayloadConcurrency

	if request.generator != nil {
		iterator := request.generator.NewIterator()
		var (
			multiErr error
			events   []*output.InternalWrappedEvent
		)
		m := &sync.Mutex{}
		swg, err := syncutil.New(syncutil.WithSize(request.Threads))
		if err != nil {
			return nil, err
		}

		for {
			value, ok := iterator.Value()
			if !ok {
				break
			}

			// resize check point - nop if there are no changes
			if shouldFollowGlobal && swg.Size != request.options.Options.PayloadConcurrency {
				swg.Resize(request.options.Options.PayloadConcurrency)
			}

			value = generators.MergeMaps(value, payloads)
			swg.Add()
			go func(vars map[string]interface{}) {
				defer swg.Done()
				ev, err := request.executeRequestWithPayloads(variables, actualAddress, address, input, shouldUseTLS, vars, previous)
				m.Lock()
				if err != nil {
					multiErr = multierr.Append(multiErr, err)
				} else {
					events = append(events, ev)
				}
				m.Unlock()
			}(value)
		}
		swg.Wait()

		return events, multiErr
	} else {
		value := maps.Clone(payloads)
		ev, err := request.executeRequestWithPayloads(variables, actualAddress, address, input, shouldUseTLS, value, previous)
		return []*output.InternalWrappedEvent{ev}, err
	}
}

func (request *Request) executeRequestWithPayloads(variables map[string]interface{}, actualAddress, address string, input *contextargs.Context, shouldUseTLS bool, payloads map[string]interface{}, previous output.InternalEvent) (*output.InternalWrappedEvent, error) {
	var (
		hostname string
		conn     net.Conn
		err      error
	)
	if host, _, err := net.SplitHostPort(actualAddress); err == nil {
		hostname = host
	}

	if shouldUseTLS {
		conn, err = request.dialer.DialTLS(context.Background(), "tcp", actualAddress)
	} else {
		conn, err = request.dialer.Dial(context.Background(), "tcp", actualAddress)
	}
	if err != nil {
		request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
		request.options.Progress.IncrementFailedRequestsBy(1)
		return nil, errors.Wrap(err, "could not connect to server")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Duration(request.options.Options.Timeout) * time.Second))

	var interactshURLs []string

	var responseBuilder, reqBuilder strings.Builder

	interimValues := generators.MergeMaps(variables, payloads)

	if vardump.EnableVarDump {
		gologger.Debug().Msgf("Network Protocol request variables: \n%s\n", vardump.DumpVariables(interimValues))
	}

	inputEvents := make(map[string]interface{})

	for _, input := range request.Inputs {
		data := []byte(input.Data)

		if request.options.Interactsh != nil {
			var transformedData string
			transformedData, interactshURLs = request.options.Interactsh.Replace(string(data), []string{})
			data = []byte(transformedData)
		}

		finalData, err := expressions.EvaluateByte(data, interimValues)
		if err != nil {
			request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
			request.options.Progress.IncrementFailedRequestsBy(1)
			return nil, errors.Wrap(err, "could not evaluate template expressions")
		}

		reqBuilder.Write(finalData)

		if err := expressions.ContainsUnresolvedVariables(string(finalData)); err != nil {
			gologger.Warning().Msgf("[%s] Could not make network request for %s: %v\n", request.options.TemplateID, actualAddress, err)
			return nil, err
		}

		if input.Type.GetType() == hexType {
			finalData, err = hex.DecodeString(string(finalData))
			if err != nil {
				request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
				request.options.Progress.IncrementFailedRequestsBy(1)
				return nil, errors.Wrap(err, "could not write request to server")
			}
		}

		if _, err := conn.Write(finalData); err != nil {
			request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
			request.options.Progress.IncrementFailedRequestsBy(1)
			return nil, errors.Wrap(err, "could not write request to server")
		}

		if input.Read > 0 {
			buffer, err := ConnReadNWithTimeout(conn, int64(input.Read), DefaultReadTimeout)
			if err != nil {
				return nil, errorutil.NewWithErr(err).Msgf("could not read response from connection")
			}

			responseBuilder.Write(buffer)

			bufferStr := string(buffer)
			if input.Name != "" {
				inputEvents[input.Name] = bufferStr
				interimValues[input.Name] = bufferStr
			}

			// Run any internal extractors for the request here and add found values to map.
			if request.CompiledOperators != nil {
				values := request.CompiledOperators.ExecuteInternalExtractors(map[string]interface{}{input.Name: bufferStr}, request.Extract)
				for k, v := range values {
					payloads[k] = v
				}
			}
		}
	}

	request.options.Progress.IncrementRequests()

	if request.options.Options.Debug || request.options.Options.DebugRequests || request.options.Options.StoreResponse {
		requestBytes := []byte(reqBuilder.String())
		msg := fmt.Sprintf("[%s] Dumped Network request for %s\n%s", request.options.TemplateID, actualAddress, hex.Dump(requestBytes))
		if request.options.Options.Debug || request.options.Options.DebugRequests {
			gologger.Info().Str("address", actualAddress).Msg(msg)
		}
		if request.options.Options.StoreResponse {
			request.options.Output.WriteStoreDebugData(address, request.options.TemplateID, request.Type().String(), msg)
		}
		if request.options.Options.VerboseVerbose {
			gologger.Print().Msgf("\nCompact HEX view:\n%s", hex.EncodeToString(requestBytes))
		}
	}

	request.options.Output.Request(request.options.TemplatePath, actualAddress, request.Type().String(), err)
	gologger.Verbose().Msgf("Sent TCP request to %s", actualAddress)

	bufferSize := 1024
	if request.ReadSize != 0 {
		bufferSize = request.ReadSize
	}
	if request.ReadAll {
		bufferSize = -1
	}

	final, err := ConnReadNWithTimeout(conn, int64(bufferSize), DefaultReadTimeout)
	if err != nil {
		request.options.Output.Request(request.options.TemplatePath, address, request.Type().String(), err)
		gologger.Verbose().Msgf("could not read more data from %s: %s", actualAddress, err)
	}
	responseBuilder.Write(final)

	response := responseBuilder.String()
	outputEvent := request.responseToDSLMap(reqBuilder.String(), string(final), response, input.MetaInput.Input, actualAddress)
	// add response fields to template context and merge templatectx variables to output event
	request.options.AddTemplateVars(input.MetaInput, request.Type(), request.ID, outputEvent)
	if request.options.HasTemplateCtx(input.MetaInput) {
		outputEvent = generators.MergeMaps(outputEvent, request.options.GetTemplateCtx(input.MetaInput).GetAll())
	}
	outputEvent["ip"] = request.dialer.GetDialedIP(hostname)
	if request.options.StopAtFirstMatch {
		outputEvent["stop-at-first-match"] = true
	}
	for k, v := range previous {
		outputEvent[k] = v
	}
	for k, v := range interimValues {
		outputEvent[k] = v
	}
	for k, v := range inputEvents {
		outputEvent[k] = v
	}
	if request.options.Interactsh != nil {
		request.options.Interactsh.MakePlaceholders(interactshURLs, outputEvent)
	}

	var event *output.InternalWrappedEvent

	switch {
	case len(interactshURLs) == 0:
		event = eventcreator.CreateEventWithAdditionalOptions(request, generators.MergeMaps(payloads, outputEvent), request.options.Options.Debug || request.options.Options.DebugResponse, func(wrappedEvent *output.InternalWrappedEvent) {
			wrappedEvent.OperatorsResult.PayloadValues = payloads
		})
		event.UsesInteractsh = len(interactshURLs) > 0
		dumpResponse(event, request, response, actualAddress, address)
		return event, nil
	case request.options.Interactsh != nil:
		event = &output.InternalWrappedEvent{InternalEvent: outputEvent}
		event.UsesInteractsh = len(interactshURLs) > 0
		request.options.Interactsh.RequestEvent(interactshURLs, &interactsh.RequestData{
			MakeResultFunc: request.MakeResultEvent,
			Event:          event,
			Operators:      request.CompiledOperators,
			MatchFunc:      request.Match,
			ExtractFunc:    request.Extract,
		})
		dumpResponse(event, request, response, actualAddress, address)
		return nil, nil
	default:
		dumpResponse(event, request, response, actualAddress, address)
		return nil, nil
	}
}

func dumpResponse(event *output.InternalWrappedEvent, request *Request, response string, actualAddress, address string) {
	cliOptions := request.options.Options
	if cliOptions.Debug || cliOptions.DebugResponse || cliOptions.StoreResponse {
		requestBytes := []byte(response)
		highlightedResponse := responsehighlighter.Highlight(event.OperatorsResult, hex.Dump(requestBytes), cliOptions.NoColor, true)
		msg := fmt.Sprintf("[%s] Dumped Network response for %s\n\n", request.options.TemplateID, actualAddress)
		if cliOptions.Debug || cliOptions.DebugResponse {
			gologger.Debug().Msg(fmt.Sprintf("%s%s", msg, highlightedResponse))
		}
		if cliOptions.StoreResponse {
			request.options.Output.WriteStoreDebugData(address, request.options.TemplateID, request.Type().String(), fmt.Sprintf("%s%s", msg, hex.Dump(requestBytes)))
		}
		if cliOptions.VerboseVerbose {
			displayCompactHexView(event, response, cliOptions.NoColor)
		}
	}
}

func displayCompactHexView(event *output.InternalWrappedEvent, response string, noColor bool) {
	operatorsResult := event.OperatorsResult
	if operatorsResult != nil {
		var allMatches []string
		for _, namedMatch := range operatorsResult.Matches {
			for _, matchElement := range namedMatch {
				allMatches = append(allMatches, hex.EncodeToString([]byte(matchElement)))
			}
		}
		tempOperatorResult := &operators.Result{Matches: map[string][]string{"matchesInHex": allMatches}}
		gologger.Print().Msgf("\nCompact HEX view:\n%s", responsehighlighter.Highlight(tempOperatorResult, hex.EncodeToString([]byte(response)), noColor, false))
	}
}

// getAddress returns the address of the host to make request to
func getAddress(toTest string) (string, error) {
	if strings.Contains(toTest, "://") {
		parsed, err := url.Parse(toTest)
		if err != nil {
			return "", err
		}
		toTest = parsed.Host
	}
	return toTest, nil
}

func ConnReadNWithTimeout(conn net.Conn, n int64, timeout time.Duration) ([]byte, error) {
	if timeout == 0 {
		timeout = DefaultReadTimeout
	}
	if n == -1 {
		// if n is -1 then read all available data from connection
		return reader.ConnReadNWithTimeout(conn, -1, timeout)
	} else if n == 0 {
		n = 4096 // default buffer size
	}
	b := make([]byte, n)
	_ = conn.SetDeadline(time.Now().Add(timeout))
	count, err := conn.Read(b)
	_ = conn.SetDeadline(time.Time{})
	if err != nil && os.IsTimeout(err) && count > 0 {
		// in case of timeout with some value read, return the value
		return b[:count], nil
	}
	if err != nil {
		return nil, err
	}
	return b[:count], nil
}
