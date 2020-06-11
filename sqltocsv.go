// sqltocsv is a package to make it dead easy to turn arbitrary database query
// results (in the form of database/sql Rows) into CSV output.
//
// Source and README at https://github.com/joho/sqltocsv
package sqltocsv

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

// WriteFile will write a CSV file to the file name specified (with headers)
// based on whatever is in the sql.Rows you pass in. It calls WriteCsvToWriter under
// the hood.
func WriteFile(csvFileName string, rows *sql.Rows) error {
	return New(rows).WriteFile(csvFileName)
}

// WriteString will return a string of the CSV. Don't use this unless you've
// got a small data set or a lot of memory
func WriteString(rows *sql.Rows) (string, error) {
	return New(rows).WriteString()
}

// Write will write a CSV file to the writer passed in (with headers)
// based on whatever is in the sql.Rows you pass in.
func Write(writer io.Writer, rows *sql.Rows) error {
	return New(rows).Write(writer)
}

// CsvPreprocessorFunc is a function type for preprocessing your CSV.
// It takes the columns after they've been munged into strings but
// before they've been passed into the CSV writer.
//
// Return an outputRow of false if you want the row skipped otherwise
// return the processed Row slice as you want it written to the CSV.
type CsvPreProcessorFunc func(row []string, columnNames []string) (outputRow bool, processedRow []string)

// Converter does the actual work of converting the rows to CSV.
// There are a few settings you can override if you want to do
// some fancy stuff to your CSV.
type Converter struct {
	Headers      []string // Column headers to use (default is rows.Columns())
	WriteHeaders bool     // Flag to output headers in your CSV (default is true)
	TimeFormat   string   // Format string for any time.Time values (default is time's default)
	Delimiter    rune     // Delimiter to use in your CSV (default is comma)

	rows            *sql.Rows
	rowPreProcessor CsvPreProcessorFunc
}

// SetRowPreProcessor lets you specify a CsvPreprocessorFunc for this conversion
func (c *Converter) SetRowPreProcessor(processor CsvPreProcessorFunc) {
	c.rowPreProcessor = processor
}

// String returns the CSV as a string in an fmt package friendly way
func (c Converter) String() string {
	csv, err := c.WriteString()
	if err != nil {
		return ""
	}
	return csv
}

// WriteString returns the CSV as a string and an error if something goes wrong
func (c Converter) WriteString() (string, error) {
	buffer := bytes.Buffer{}
	err := c.Write(&buffer)
	return buffer.String(), err
}

// WriteFile writes the CSV to the filename specified, return an error if problem
func (c Converter) WriteFile(csvFileName string) error {
	f, err := os.Create(csvFileName)
	if err != nil {
		return err
	}

	err = c.Write(f)
	if err != nil {
		f.Close() // close, but only return/handle the write error
		return err
	}

	return f.Close()
}

// Write writes the CSV to the Writer provided
func (c Converter) Write(writer io.Writer) error {
	log.Println("In write function")
	rows := c.rows

	//var b bytes.Buffer
	b := bytes.NewBuffer(make([]byte, 0, 50000))
	csvWriter := csv.NewWriter(b)

	zw := gzip.NewWriter(writer)
	defer zw.Close()

	if c.Delimiter != '\x00' {
		csvWriter.Comma = c.Delimiter
	}

	columnNames, err := rows.Columns()
	if err != nil {
		return err
	}

	log.Println(rows.Next())
	if c.WriteHeaders {
		log.Println("writing headers")
		// use Headers if set, otherwise default to
		// query Columns
		var headers []string
		if len(c.Headers) > 0 {
			headers = c.Headers
		} else {
			headers = columnNames
		}
		err = csvWriter.Write(headers)
		if err != nil {
			return err
		}
		log.Println(rows.Next())
		log.Println("Flushing")
		//csvWriter.Flush()
		//err = csvWriter.Error()
		//if err != nil {
		//	log.Println(err)
		//	return err
		//}
		log.Println(b.Len(), b.Cap())
		log.Println("zipping")
		_, err = zw.Write(b.Bytes())
		if err != nil {
			log.Println(err)
			return err
		}
		log.Println(b.Len(), b.Cap())
		log.Println(rows.Next())
	}

	log.Println("zipping")
	count := len(columnNames)
	values := make([]interface{}, count)
	valuePtrs := make([]interface{}, count)

	log.Println(rows.Err())
	log.Println(rows.Next())
	for rows.Next() {
		log.Println("in for loop")
		row := make([]string, count)

		for i, _ := range columnNames {
			valuePtrs[i] = &values[i]
		}

		if err = rows.Scan(valuePtrs...); err != nil {
			return err
		}

		for i, _ := range columnNames {
			var value interface{}
			rawValue := values[i]

			byteArray, ok := rawValue.([]byte)
			if ok {
				value = string(byteArray)
			} else {
				value = rawValue
			}

			timeValue, ok := value.(time.Time)
			if ok && c.TimeFormat != "" {
				value = timeValue.Format(c.TimeFormat)
			}

			if value == nil {
				row[i] = ""
			} else {
				row[i] = fmt.Sprintf("%v", value)
			}
		}

		writeRow := true
		if c.rowPreProcessor != nil {
			writeRow, row = c.rowPreProcessor(row, columnNames)
		}
		if writeRow {
			err = csvWriter.Write(row)
			if err != nil {
				return err
			}
			csvWriter.Flush()
			err = csvWriter.Error()
			if err != nil {
				return err
			}
			log.Println(b.String())
			_, err = zw.Write(b.Bytes())
			if err != nil {
				return err
			}
		}
	}
	err = rows.Err()
	if err != nil {
		return err
	}

	csvWriter.Flush()
	err = csvWriter.Error()
	if err != nil {
		return err
	}
	log.Println(b.String())
	_, err = zw.Write(b.Bytes())
	if err != nil {
		return err
	}
	b.Reset()

	return err
}

// New will return a Converter which will write your CSV however you like
// but will allow you to set a bunch of non-default behaivour like overriding
// headers or injecting a pre-processing step into your conversion
func New(rows *sql.Rows) *Converter {
	return &Converter{
		rows:         rows,
		WriteHeaders: true,
		Delimiter:    ',',
	}
}
