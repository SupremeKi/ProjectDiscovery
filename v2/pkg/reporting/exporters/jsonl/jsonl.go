package jsonl

import (
	"encoding/json"
	"github.com/pkg/errors"
	"github.com/projectdiscovery/nuclei/v2/pkg/output"
	"os"
	"sync"
)

type Exporter struct {
	options *Options
	mutex   *sync.Mutex
	rows    []output.ResultEvent
}

// Options contains the configuration options for JSONL exporter client
type Options struct {
	// File is the file to export found JSONL result to
	File string `yaml:"file"`
}

// New creates a new JSONL exporter integration client based on options.
func New(options *Options) (*Exporter, error) {
	exporter := &Exporter{
		mutex:   &sync.Mutex{},
		options: options,
		rows:    []output.ResultEvent{},
	}
	return exporter, nil
}

// Export appends the passed result event to the list of objects to be exported to
// the resulting JSONL file
func (exporter *Exporter) Export(event *output.ResultEvent) error {
	exporter.mutex.Lock()
	defer exporter.mutex.Unlock()

	// Add the event to the rows
	exporter.rows = append(exporter.rows, *event)

	return nil
}

// Close writes the in-memory data to the JSONL file specified by options.JSONLExport
// and closes the exporter after operation
func (exporter *Exporter) Close() error {
	exporter.mutex.Lock()
	defer exporter.mutex.Unlock()

	// Open the JSONL file for writing and create it if it doesn't exist
	f, err := os.OpenFile(exporter.options.File, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errors.Wrap(err, "failed to create JSONL file")
	}

	// Loop through the rows and convert each to a JSON byte array and write to file
	for _, row := range exporter.rows {
		// Convert the row to JSON byte array
		obj, err := json.Marshal(row)
		if err != nil {
			return errors.Wrap(err, "failed to generate row for JSONL report")
		}

		// Attempt to append the JSON line to file specified in options.JSONExport
		if _, err = f.Write(obj); err != nil {
			return errors.Wrap(err, "failed to append JSONL line")
		}
	}

	// Close the file
	if err := f.Close(); err != nil {
		return errors.Wrap(err, "failed to close JSONL file")
	}

	return nil
}
