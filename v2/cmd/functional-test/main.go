package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/logrusorgru/aurora"
	"github.com/pkg/errors"

	"github.com/projectdiscovery/nuclei/v2/pkg/testutils"
)

var (
	debug        = os.Getenv("DEBUG") == "true"
	githubAction = os.Getenv("GH_ACTION") == "true"

	success = aurora.Green("[✓]").String()
	failed  = aurora.Red("[✘]").String()
	errored = false

	mainNucleiBinary = flag.String("main", "", "Main Branch Nuclei Binary")
	devNucleiBinary  = flag.String("dev", "", "Dev Branch Nuclei Binary")
	testcases        = flag.String("testcases", "", "Test cases file for nuclei functional tests")
)

func main() {
	flag.Parse()

	if err := runFunctionalTests(); err != nil {
		log.Fatalf("Could not run functional tests: %s\n", err)
	}
	if errored {
		os.Exit(1)
	}
}

func runFunctionalTests() error {
	file, err := os.Open(*testcases)
	if err != nil {
		return errors.Wrap(err, "could not open test cases")
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		testCase := strings.TrimSpace(scanner.Text())
		if testCase == "" {
			continue
		}
		execute(testCase)
	}
	return nil
}

func execute(text string) {
	ghActionGroupStart := ""
	ghActionGroupEnd := ""
	if githubAction {
		ghActionGroupStart = "::group::"
		ghActionGroupEnd = "::endgroup::"
	}

	if err := runIndividualTestCase(text); err != nil {
		errored = true
		fmt.Fprintf(os.Stderr, "%s%s Test \"%s\" failed: %s\n%s", ghActionGroupStart, failed, text, err, ghActionGroupEnd)
	} else {
		fmt.Printf("%s%s Test \"%s\" passed!\n%s", ghActionGroupStart, success, text, ghActionGroupEnd)
	}
}

func runIndividualTestCase(testcase string) error {
	parts := strings.Fields(testcase)

	var finalArgs []string
	if len(parts) > 1 {
		finalArgs = parts[1:]
	}
	mainOutput, err := testutils.RunNucleiBinaryAndGetLoadedTemplates(*mainNucleiBinary, debug, finalArgs)
	if err != nil {
		return errors.Wrap(err, "could not run nuclei main test")
	}
	devOutput, err := testutils.RunNucleiBinaryAndGetLoadedTemplates(*devNucleiBinary, debug, finalArgs)
	if err != nil {
		return errors.Wrap(err, "could not run nuclei dev test")
	}
	if mainOutput == devOutput {
		return nil
	}
	return fmt.Errorf("%s main is not equal to %s dev", mainOutput, devOutput)
}
