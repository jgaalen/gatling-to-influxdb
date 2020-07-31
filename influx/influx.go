/*
Copyright © 2019 Anton Kramarev

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package influx

import (
	"context"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/dakaraj/gatling-to-influxdb/logger"
	"github.com/dakaraj/gatling-to-influxdb/types"
	_ "github.com/influxdata/influxdb1-client" // workaround from client documentation
	infc "github.com/influxdata/influxdb1-client/v2"
	"github.com/spf13/cobra"
)

type testInfo struct {
	testID         string
	simulationName string
	description    string
	nodeName       string
	testStartTime  time.Time
}

type syncUsers struct {
	m sync.Mutex
	u map[string]int
}

func (s *syncUsers) GetSnapshot() map[string]int {
	m := make(map[string]int)
	s.m.Lock()
	for k, v := range s.u {
		m[k] = v
	}
	s.m.Unlock()

	return m
}

const (
	userMetricsTicker = 1
)

var (
	c         infc.Client
	l         *log.Logger
	dbName    string
	info      testInfo
	lastPoint time.Time

	// users is a thread safe map for storing current snapshot of users
	// amount generated
	users = syncUsers{
		m: sync.Mutex{},
		u: make(map[string]int),
	}

	// pc is a channel to send all point from parser to
	pc = make(chan *infc.Point, 1000)

	// TODO: parameterize later
	maxPoints        uint
	writeDataTimeout = 10
)

// InitTestInfo collect basic test information to be used by Influx client
func InitTestInfo(testID, simulationName, description, nodeName string, testStartTime time.Time) {
	info = testInfo{
		testID:         testID,
		simulationName: simulationName,
		description:    description,
		nodeName:       nodeName,
		testStartTime:  testStartTime,
	}
}

// NewPoint is mostly an alias fo standard NewPoint function from influx package,
// except timestamp is required
func NewPoint(name string, tags map[string]string, fields map[string]interface{}, t time.Time) (*infc.Point, error) {
	return infc.NewPoint(name, tags, fields, t)
}

// SendPoint sends point to the channel listened by metrics consumer
func SendPoint(p *infc.Point) {
	pc <- p
}

// IncUsersKey increments amount of users for given scenario
func IncUsersKey(scenario string) {
	users.m.Lock()
	users.u[scenario]++
	users.m.Unlock()
}

// DecUsersKey decrements amount of users for given scenario
func DecUsersKey(scenario string) {
	users.m.Lock()
	defer users.m.Unlock()
	if users.u[scenario] == 1 {
		delete(users.u, scenario)
		return
	}
	users.u[scenario]--
}

func sendBatch(points []*infc.Point) {
	const retries = 5

	bp, _ := infc.NewBatchPoints(infc.BatchPointsConfig{
		Precision: "ns",
		Database:  dbName,
	})
	bp.AddPoints(points)

	// Retry mechanism for batch points sending
	var errCounter int
SendLoop:
	for {
		err := c.Write(bp)
		if err != nil {
			l.Printf("Error sending points batch to InfluxDB: %v\n", err)
			errCounter++
			if errCounter == retries {
				l.Printf("Failed to send %d points as batch to server\n", len(points))
				return
			}
			time.Sleep(2 * time.Second)
		}
		break SendLoop
	}

	if errCounter > 0 {
		l.Printf("%d points successfully sent after %d retries\n", len(points), errCounter)
		return
	}

	l.Printf("Successfully written %d points to DB\n", len(points))
}

// SendUserLineData takes a line with user data and adds it to the processing list
func SendUserLineData(uld *types.UserLineData) {
	_ = uld
}

func usersProcessor(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	ticker := time.NewTicker(time.Second * userMetricsTicker)
	defer ticker.Stop()
GatherLoop:
	for {
		select {
		case t := <-ticker.C:
			snap := users.GetSnapshot()
			if len(snap) > 0 {
				for k, v := range snap {
					p, _ := infc.NewPoint(
						"users",
						map[string]string{
							"scenario": k,
							"testId":   info.testID,
						},
						map[string]interface{}{
							"active":   v,
							"nodeName": info.nodeName,
						},
						t,
					)
					pc <- p
				}
			}
		case <-ctx.Done():
			break GatherLoop
		}
	}
}

func metricsPointsCollector(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	points := make([]*infc.Point, 0, int(maxPoints))

	timer := time.NewTimer(time.Second * time.Duration(writeDataTimeout))
CollectorLoop:
	for {
		select {
		// Send points after timer expires
		case <-timer.C:
			if len(points) > 0 {
				sendBatch(points)
				// After sending points to server clear points buffer
				points = make([]*infc.Point, 0, int(maxPoints))
			}
			// Reset timer
			timer.Reset(time.Second * time.Duration(writeDataTimeout))
		// When point is received on the channel
		case p := <-pc:
			points = append(points, p)
			// Send batch points when batch capacity is reached
			if len(points) == int(maxPoints) {
				sendBatch(points)
				// After sending points to server clear points buffer
				points = make([]*infc.Point, 0, maxPoints)
				// Reset timer
				timer.Reset(time.Second * time.Duration(writeDataTimeout))
			}
			// Each point received saves its timestamp for use as a closing point
			lastPoint = p.Time()
		// Await for external stop signal
		case <-ctx.Done():
			// Send any unsent points
			if len(points) > 0 {
				sendBatch(points)
				points = make([]*infc.Point, 0, int(maxPoints))
			}
			break CollectorLoop
		}
	}
}

func sendClosingPoint() {
	// If info struct is empty, then parsing of file did not start,
	// so there is no need to send closing point
	if info.testID == "" {
		l.Println("Skipping stop test point write...")
		return
	}

	// Create a point signifying a test end
	p, _ := infc.NewPoint(
		"testStartEnd",
		map[string]string{
			"action":         "finish",
			"testId":         info.testID,
			"simulationName": info.simulationName,
		},
		map[string]interface{}{
			"description": info.description,
			"nodeName":    info.nodeName,
		},
		// Add 5 secods to the time since last point was received
		lastPoint.Add(time.Second*5),
	)

	sendBatch([]*infc.Point{p})
}

// StartProcessing starts consumers that receive points from parser and send to
// InfluxDB server
func StartProcessing(ctx context.Context, owg *sync.WaitGroup) {
	defer owg.Done()

	l.Println("Starting consumers for parser results")
	wg := &sync.WaitGroup{}

	// start requests consumer
	upCtx, upCancel := context.WithCancel(context.Background())
	mpcCtx, mpcCancel := context.WithCancel(context.Background())
	wg.Add(2)
	go usersProcessor(upCtx, wg)
	go metricsPointsCollector(mpcCtx, wg)

	// Wait for external stop signal
	<-ctx.Done()

	l.Println("Stopping all consumers...")
	upCancel()
	mpcCancel() // This should be the last one

	wg.Wait()
	sendClosingPoint()
	l.Println("Finishing process")
}

// InitInfluxConnection checks if connection with InfluxDB is successful
func InitInfluxConnection(cmd *cobra.Command) error {
	// Getting logger for package
	l = logger.GetLogger()

	username, _ := cmd.Flags().GetString("username")
	password, _ := cmd.Flags().GetString("password")
	address, _ := cmd.Flags().GetString("address")
	dbName, _ = cmd.Flags().GetString("database")
	maxPoints, _ = cmd.Flags().GetUint("max-batch-size")

	var err error
	c, err = infc.NewHTTPClient(infc.HTTPConfig{
		Addr:      address,
		Username:  username,
		Password:  password,
		UserAgent: fmt.Sprintf("g2i-http-client-%s(%s)", cmd.Version, runtime.Version()),
		Timeout:   time.Second * 60,
	})
	if err != nil {
		return err
	}

	_, _, err = c.Ping(time.Second * 10)
	if err != nil {
		return fmt.Errorf("Connection with InfluxDB at %s could not be established. Error: %w", address, err)
	}
	res, err := c.Query(infc.NewQuery("SHOW MEASUREMENTS", dbName, ""))
	if err != nil {
		return fmt.Errorf("Connection with InfluxDB at %s could not be established. Error: %w", address, err)
	}
	if err := res.Error(); err != nil {
		return fmt.Errorf("Test query failed with error: %w", err)
	}
	l.Printf("Connection with InfluxDB at %s successfully established\n", address)

	return nil
}
