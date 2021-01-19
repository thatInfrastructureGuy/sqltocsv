// sqltocsvgzip package converts database query results
// (in the form of database/sql Rows) into CSV.GZIP output.
//
// Source and README at https://github.com/thatInfrastructureGuy/sqltocsvgzip
package sqltocsvgzip

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// WriteFile will write a CSV.GZIP file to the file name specified (with headers)
// based on whatever is in the sql.Rows you pass in.
func WriteFile(csvGzipFileName string, rows *sql.Rows) (rowCount int64, err error) {
	return WriteConfig(rows).WriteFile(csvGzipFileName)
}

// UploadToS3 will upload a CSV.GZIP file to AWS S3 bucket (with headers)
// based on whatever is in the sql.Rows you pass in.
// UploadToS3 looks for the following environment variables.
// Required: S3_BUCKET, S3_PATH, S3_REGION
// Optional: S3_ACL (default => bucket-owner-full-control)
func UploadToS3(rows *sql.Rows) (rowCount int64, err error) {
	return UploadConfig(rows).Upload()
}

// Upload uploads the csv.gzip, return an error if problem.
// Creates a Multipart AWS requests.
// Completes the multipart request if all uploads are successful.
// Aborts the operation when an error is received.
func (c *Converter) Upload() (rowCount int64, err error) {
	if c.UploadPartSize < minFileSize {
		return 0, fmt.Errorf("UploadPartSize should be greater than %v\n", minFileSize)
	}

	// Create MultiPart S3 Upload
	err = c.createS3Session()
	if err != nil {
		return 0, err
	}

	err = c.createMultipartRequest()
	if err != nil {
		return 0, err
	}

	wg := sync.WaitGroup{}
	buf := bytes.Buffer{}
	c.uploadQ = make(chan *obj, c.UploadThreads)
	c.quit = make(chan bool, 1)

	// Upload Parts to S3
	for i := 0; i < c.UploadThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = c.UploadPart()
			if err != nil {
				c.writeLog(Error, err.Error())
			}
		}()
	}

	err = c.Write(&buf)
	if err != nil {
		// Abort S3 Upload
		awserr := c.abortMultipartUpload()
		if awserr != nil {
			return 0, awserr
		}
		return 0, err
	}

	close(c.uploadQ)
	wg.Wait()

	if c.partNumber == 0 {
		// Upload one time
		c.writeLog(Info, "Gzip file < 5 MB. Enable direct upload. Abort multipart upload.")
		err = c.abortMultipartUpload()
		if err != nil {
			return 0, err
		}

		err = c.UploadObjectToS3(&buf)
		if err != nil {
			return 0, err
		}
		return c.RowCount, nil
	}

	// Sort completed parts
	c.sortCompletedParts()
	// Complete S3 upload
	completeResponse, err := c.completeMultipartUpload()
	if err != nil {
		// Abort S3 Upload
		awserr := c.abortMultipartUpload()
		if awserr != nil {
			return 0, awserr
		}
		return 0, err
	}

	uploadPath, err := url.PathUnescape(completeResponse.String())
	if err != nil {
		return 0, err
	}
	c.writeLog(Info, "Successfully uploaded file: "+uploadPath)

	return c.RowCount, nil
}

// WriteFile writes the csv.gzip to the filename specified, return an error if problem
func (c *Converter) WriteFile(csvGzipFileName string) (rowCount int64, err error) {
	f, err := os.Create(csvGzipFileName)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Explicitely unset s3 upload
	c.S3Upload = false

	err = c.Write(f)
	if err != nil {
		return 0, err
	}

	return c.RowCount, nil
}

// Write writes the csv.gzip to the Writer provided
func (c *Converter) Write(w io.Writer) error {
	writeRow := true
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	csvWriter, csvBuffer := c.getCSVWriter()

	// Set headers
	columnNames, totalColumns, err := c.setCSVHeaders(csvWriter)
	if err != nil {
		return err
	}

	// Buffers for each iteration
	values := make([]interface{}, totalColumns, totalColumns)
	valuePtrs := make([]interface{}, totalColumns, totalColumns)

	for i := range columnNames {
		valuePtrs[i] = &values[i]
	}

	zw, err := c.getGzipWriter(w)
	if err != nil {
		return err
	}
	defer zw.Close()

	// Iterate over sql rows
	for c.rows.Next() {
		select {
		case <-c.quit:
			c.abortMultipartUpload()
			return fmt.Errorf("Received quit signal. Exiting.")
		case <-interrupt:
			c.abortMultipartUpload()
			return fmt.Errorf("Received quit signal. Exiting.")
		default:
			// Do nothing
		}

		if err = c.rows.Scan(valuePtrs...); err != nil {
			return err
		}

		row := c.stringify(values)

		if c.rowPreProcessor != nil {
			writeRow, row = c.rowPreProcessor(row, columnNames)
		}

		if writeRow {
			c.RowCount = c.RowCount + 1

			// Write to CSV Buffer
			err = csvWriter.Write(row)
			if err != nil {
				return err
			}
			csvWriter.Flush()

			// Convert from csv to gzip
			// Writes from buffer to underlying file
			if csvBuffer.Len() >= (c.GzipBatchPerGoroutine * c.GzipGoroutines) {
				_, err = zw.Write(csvBuffer.Bytes())
				if err != nil {
					return err
				}
				err = zw.Flush()
				if err != nil {
					return err
				}

				// Reset buffer
				csvBuffer.Reset()

				// Upload partially created file to S3
				// If size of the gzip file exceeds maxFileStorage
				if c.S3Upload {
					// GZIP writer to underline file.csv.gzip
					gzipBuffer, ok := w.(*bytes.Buffer)
					if !ok {
						return fmt.Errorf("Expected buffer. Got %T", w)
					}

					if gzipBuffer.Len() >= c.UploadPartSize {
						if c.partNumber == 10000 {
							return fmt.Errorf("Number of parts cannot exceed 10000. Please increase UploadPartSize and try again.")
						}

						// Add to Queue
						c.AddToQueue(gzipBuffer, false)

						//Reset writer
						gzipBuffer.Reset()
					}
				}
			}
		}
	}

	err = c.rows.Err()
	if err != nil {
		return err
	}

	_, err = zw.Write(csvBuffer.Bytes())
	if err != nil {
		return err
	}
	err = zw.Close()
	if err != nil {
		return err
	}

	//Wipe the buffer
	csvBuffer.Reset()

	// Upload last part of the file to S3
	if c.S3Upload {
		if c.partNumber == 0 {
			return nil
		}

		// GZIP writer to underline file.csv.gzip
		gzipBuffer, ok := w.(*bytes.Buffer)
		if !ok {
			return fmt.Errorf("Expected buffer. Got %T", w)
		}

		// Add to Queue for multipart upload
		c.AddToQueue(gzipBuffer, true)

		//Reset writer
		gzipBuffer.Reset()
	}

	// Log the total number of rows processed.
	c.writeLog(Info, fmt.Sprintf("Total sql rows processed: %v", c.RowCount))
	return nil
}

// AddToQueue sends obj over the upload queue.
// Currently, It is designed to work with AWS multipart upload.
// If the part body is less than 5Mb in size, 2 parts are combined together before sending.
func (c *Converter) AddToQueue(buf *bytes.Buffer, lastPart bool) {
	// Increament PartNumber
	c.partNumber++

	if buf.Len() >= minFileSize {
		if c.partNumber > 1 {
			// Add part to queue
			c.writeLog(Debug, fmt.Sprintf("Add part to queue: #%v", c.partNumber-1))
			c.uploadQ <- &obj{
				partNumber: c.partNumber - 1,
				buf:        c.gzipBuf,
			}
		}

		c.gzipBuf = make([]byte, buf.Len())
		copy(c.gzipBuf, buf.Bytes())
		if lastPart {
			// Add last part to queue
			c.writeLog(Debug, fmt.Sprintf("Add part to queue: #%v", c.partNumber))
			c.uploadQ <- &obj{
				partNumber: c.partNumber,
				buf:        c.gzipBuf,
			}
			c.gzipBuf = c.gzipBuf[:0]
		}
	} else {
		c.writeLog(Debug, fmt.Sprintf("Buffer size %v less than %v. Appending to previous part.", buf.Len(), c.UploadPartSize))
		c.gzipBuf = append(c.gzipBuf, buf.Bytes()...)

		// Add part to queue
		c.writeLog(Debug, fmt.Sprintf("Add part to queue: #%v", c.partNumber-1))
		c.uploadQ <- &obj{
			partNumber: c.partNumber - 1,
			buf:        c.gzipBuf,
		}
		c.gzipBuf = c.gzipBuf[:0]

		c.partNumber--
	}
}

// UploadPart listens to upload queue. Whenever an obj is received,
// it is then uploaded to AWS.
// Abort operation is called if any error is received.
func (c *Converter) UploadPart() (err error) {
	mu := &sync.RWMutex{}
	for s3obj := range c.uploadQ {
		err = c.uploadPart(s3obj.partNumber, s3obj.buf, mu)
		if err != nil {
			c.writeLog(Error, "Error occurred. Sending quit signal to writer.")
			c.quit <- true
			c.abortMultipartUpload()
			return err
		}
	}
	c.writeLog(Debug, "Received closed signal")
	return
}

// writeLog decides whether to write a log to stdout depending on LogLevel.
func (c *Converter) writeLog(logLevel LogLevel, logLine string) {
	if logLevel <= c.LogLevel {
		log.Println(logLine)
	}
}
