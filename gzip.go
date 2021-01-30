package sqltocsvgzip

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/pgzip"
)

func (c *Converter) getGzipWriter(writer io.Writer) (*pgzip.Writer, error) {
	// Use pgzip for multi-threaded
	zw, err := pgzip.NewWriterLevel(writer, c.CompressionLevel)
	if err != nil {
		return zw, err
	}
	err = zw.SetConcurrency(c.GzipBatchPerGoroutine, c.GzipGoroutines)
	return zw, err
}

func (c *Converter) csvToGzip(toGzip chan csvBuf, w io.Writer, wg *sync.WaitGroup) {
	defer wg.Done()
	var gzipBuffer *bytes.Buffer
	if c.S3Upload {
		var ok bool
		gzipBuffer, ok = w.(*bytes.Buffer)
		if !ok {
			c.quit <- fmt.Errorf("Expected buffer. Got %T", w)
			return
		}
	}

	// GZIP writer to underline file.csv.gzip
	zw, err := c.getGzipWriter(w)
	if err != nil {
		c.quit <- fmt.Errorf("Error creating gzip writer: %v", err)
		return
	}
	defer zw.Close()

	for csvBuf := range toGzip {
		_, err = zw.Write(csvBuf.data)
		if err != nil {
			c.quit <- fmt.Errorf("Error writing to gzip buffer: %v", err)
			return
		}

		if csvBuf.lastPart {
			err = zw.Close()
			if err != nil {
				c.quit <- fmt.Errorf("Error flushing contents to gzip writer: %v", err)
				return
			}
		} else {
			err = zw.Flush()
			if err != nil {
				c.quit <- fmt.Errorf("Error flushing contents to gzip writer: %v", err)
				return
			}
		}

		// Upload partially created file to S3
		// If size of the gzip file exceeds maxFileStorage
		if c.S3Upload {
			if csvBuf.lastPart || gzipBuffer.Len() >= c.UploadPartSize {
				if c.partNumber == 10000 {
					c.quit <- fmt.Errorf("Number of parts cannot exceed 10000. Please increase UploadPartSize and try again.")
					return
				}

				// Add to Queue
				c.AddToQueue(gzipBuffer, csvBuf.lastPart)

				//Reset writer
				gzipBuffer.Reset()
			}
		}
	}

	// Close channel after sending complete.
	if c.S3Upload {
		close(c.uploadQ)
	}
}
