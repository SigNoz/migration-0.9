package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
)

const samplesTable = "samples"
const timeSeriesTable = "time_series"
const samplesTableV2 = "samples_v2"
const timeSeriesTableV2 = "time_series_v2"

var fingerprintToName map[uint64]string

type DBResponseTotal struct {
	NumTotal uint64 `ch:"numTotal"`
}

type Samples struct {
	TimeStamp   int64   `ch:"timestamp_ms"`
	Fingerprint uint64  `ch:"fingerprint"`
	Value       float64 `ch:"value"`
}

type TimeSeries struct {
	MetricName  string    `ch:"metric_name"`
	Date        time.Time `ch:"date"`
	Fingerprint uint64    `ch:"fingerprint"`
	Labels      string    `ch:"labels"`
}

type SamplesV2 struct {
	MetricsName string  `ch:"metric_name"`
	TimeStamp   int64   `ch:"timestamp_ms"`
	Fingerprint uint64  `ch:"fingerprint"`
	Value       float64 `ch:"value"`
}

type TimeSeriesV2 struct {
	MetricName   string    `ch:"metric_name"`
	Date         time.Time `ch:"date"`
	Fingerprint  uint64    `ch:"fingerprint"`
	Labels       string    `ch:"labels"`
	LabelsObject string    `ch:"labels_object"`
}

func connect(host string, port string, userName string, password string, database string) (clickhouse.Conn, error) {
	var (
		ctx       = context.Background()
		conn, err = clickhouse.Open(&clickhouse.Options{
			Addr: []string{fmt.Sprintf("%s:%s", host, port)},
			Auth: clickhouse.Auth{
				Database: database,
				Username: userName,
				Password: password,
			},
			//Debug:           true,
		})
	)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		if exception, ok := err.(*clickhouse.Exception); ok {
			fmt.Printf("Catch exception [%d] %s \n%s\n", exception.Code, exception.Message, exception.StackTrace)
		}
		return nil, err
	}
	return conn, nil
}

func readTotalRowsSamples(conn clickhouse.Conn) (uint64, error) {
	ctx := context.Background()
	result := []DBResponseTotal{}
	if err := conn.Select(ctx, &result, fmt.Sprintf("SELECT count() as numTotal FROM %s", samplesTable)); err != nil {
		return 0, err
	}
	fmt.Println("Total Rows: ", result[0].NumTotal)
	return result[0].NumTotal, nil
}

func readTotalRowsTimeSeries(conn clickhouse.Conn) (uint64, error) {
	ctx := context.Background()
	result := []DBResponseTotal{}
	if err := conn.Select(ctx, &result, fmt.Sprintf("SELECT count() as numTotal FROM %s", timeSeriesTable)); err != nil {
		return 0, err
	}
	fmt.Println("Total Rows: ", result[0].NumTotal)
	return result[0].NumTotal, nil
}

func prepareTimeSeries(conn clickhouse.Conn) ([]TimeSeriesV2, error) {
	fingerprintToName = make(map[uint64]string)
	ctx := context.Background()
	result := []TimeSeriesV2{}
	query := fmt.Sprintf("SELECT JSONExtractString(labels, '__name__') as metric_name, fingerprint, date, labels FROM %s", timeSeriesTable)
	if err := conn.Select(ctx, &result, query); err != nil {
		return nil, err
	}
	for _, item := range result {
		fingerprintToName[item.Fingerprint] = item.MetricName
	}
	return result, nil
}

func prepareSamples(conn clickhouse.Conn) ([]SamplesV2, error) {
	ctx := context.Background()
	result := []Samples{}
	query := fmt.Sprintf("SELECT * FROM %s", samplesTable)
	if err := conn.Select(ctx, &result, query); err != nil {
		return nil, err
	}
	newResult := []SamplesV2{}
	for idx := range result {
		item := result[idx]
		name := fingerprintToName[item.Fingerprint]
		newItem := SamplesV2{}

		newItem.MetricsName = string([]byte(name))
		newItem.Fingerprint = item.Fingerprint
		newItem.TimeStamp = item.TimeStamp
		newItem.Value = item.Value
		newResult = append(newResult, newItem)
	}
	return newResult, nil
}

func writeSamples(conn clickhouse.Conn, batchSamples []SamplesV2) error {
	ctx := context.Background()
	statement, err := conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s", samplesTableV2))
	if err != nil {
		return err
	}
	for i, sample := range batchSamples {
		if i%1000 == 0 {
			fmt.Printf("At %d the sample batch\n", i)
		}
		err = statement.Append(
			sample.MetricsName,
			sample.Fingerprint,
			sample.TimeStamp,
			sample.Value,
		)
		if err != nil {
			return err
		}
	}

	return statement.Send()
}

func writeTimeSeries(conn clickhouse.Conn, batchSeries []TimeSeriesV2) error {
	ctx := context.Background()
	err := conn.Exec(ctx, `SET allow_experimental_object_type = 1`)
	if err != nil {
		return err
	}

	statement, err := conn.PrepareBatch(ctx, fmt.Sprintf("INSERT INTO %s (metric_name, date, fingerprint, labels) VALUES (?, ?, ?, ?)", timeSeriesTableV2))
	if err != nil {
		return err
	}
	for _, series := range batchSeries {
		err = statement.Append(
			series.MetricName,
			series.Date,
			series.Fingerprint,
			series.Labels,
		)
		if err != nil {
			return err
		}
	}

	return statement.Send()
}

func moveTimeSeries(conn clickhouse.Conn) error {
	ctx := context.Background()

	query := fmt.Sprintf(`
		INSERT INTO
		%s
		SELECT
			JSONExtractString(labels, '__name__') as metric_name, date, fingerprint, labels, labels as labels_object
		FROM %s
	`, timeSeriesTableV2, timeSeriesTable)
	if err := conn.Exec(ctx, query); err != nil {
		return err
	}
	return nil
}

func dropOldTables(conn clickhouse.Conn) {
	ctx := context.Background()
	fmt.Println("Dropping samples table")
	err := conn.Exec(ctx, "DROP TABLE IF EXISTS samples")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Successfully dropped samples")
	fmt.Println("Dropping time_series table")
	err = conn.Exec(ctx, "DROP TABLE IF EXISTS time_series")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Successfully dropped time_series")
}

func main() {
	start := time.Now()
	hostFlag := flag.String("host", "127.0.0.1", "clickhouse host")
	portFlag := flag.String("port", "9000", "clickhouse port")
	userNameFlag := flag.String("userName", "default", "clickhouse username")
	passwordFlag := flag.String("password", "", "clickhouse password")
	databaseFlag := flag.String("database", "signoz_metrics", "metrics database")
	dropOldTable := flag.Bool("dropOldTable", false, "clear old clickhouse data if migration was successful")
	dataSource := flag.String("dataSource", "signoz.db", "Data Source path")
	flag.Parse()
	fmt.Println(*hostFlag, *portFlag, *userNameFlag, *passwordFlag, *databaseFlag)

	conn, err := connect(*hostFlag, *portFlag, *userNameFlag, *passwordFlag, *databaseFlag)
	if err != nil {
		log.Fatalf("Error while making connection: %s", err)
	}

	rows, err := readTotalRowsSamples(conn)
	if err != nil {
		log.Fatalf("Error while reading total sample rows: %s", err)
	}
	fmt.Printf("There are total %v samples rows, starting migration... \n", rows)

	rows, err = readTotalRowsTimeSeries(conn)
	if err != nil {
		log.Fatalf("Error while reading total time series rows: %s", err)
	}
	fmt.Printf("There are total %v time series rows, starting migration... \n", rows)

	_, err = prepareTimeSeries(conn)
	if err != nil {
		log.Fatalf("Error while preparing time series: %s", err)
	}

	samples, err := prepareSamples(conn)
	if err != nil {
		log.Fatalf("Error while preparing samples: %s", err)
	}
	fmt.Println("Writing samples to DB")
	err = writeSamples(conn, samples)
	if err != nil {
		log.Fatalln("Error while writing samples to DB", err)
	}

	fmt.Println("Written samples to DB")

	// Throwing unsupported column error, so we use clickhouse itself to move data

	moveTimeSeries(conn)
	if err != nil {
		log.Fatalln("Error while moving data to time series v2", err)
	}

	// fmt.Println("Writing time series")

	// err = writeTimeSeries(conn, timeSeries)
	// if err != nil {
	// 	log.Fatalln(err)
	// }
	// fmt.Println("Written time series")

	fmt.Println("Completed migration in: ", time.Since(start))
	if *dropOldTable {
		dropOldTables(conn)
	}

	fmt.Println("Data Source path: ", *dataSource)

	if _, err := os.Stat(*dataSource); os.IsNotExist(err) {
		log.Fatalf("data source file does not exist: %s", *dataSource)
	}

	// inialize database
	err = initDB(*dataSource)
	if err != nil {
		log.Fatalln(err)
	}

	// migrate dashboards
	migrateDashboards()
}
