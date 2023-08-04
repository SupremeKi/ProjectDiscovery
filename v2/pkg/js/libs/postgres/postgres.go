package postgres

import (
	"database/sql"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-pg/pg"
	_ "github.com/lib/pq"
	"github.com/praetorian-inc/fingerprintx/pkg/plugins"
	postgres "github.com/praetorian-inc/fingerprintx/pkg/plugins/services/postgresql"
	"github.com/projectdiscovery/nuclei/v2/pkg/js/scripts/utils"
)

// Client is a client for Postgres database.
//
// Internally client uses go-pg/pg driver.
type Client struct{}

// IsPostgres checks if the given host and port are running Postgres database.
//
// If connection is successful, it returns true.
// If connection is unsuccessful, it returns false and error.
func (c *Client) IsPostgres(host string, port int) (bool, error) {
	timeout := 10 * time.Second

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), timeout)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	plugin := &postgres.POSTGRESPlugin{}
	service, err := plugin.Run(conn, timeout, plugins.Target{Host: host})
	if err != nil {
		return false, err
	}
	if service == nil {
		return false, nil
	}
	return true, nil
}

// Connect connects to Postgres database using given credentials.
//
// If connection is successful, it returns true.
// If connection is unsuccessful, it returns false and error.
//
// The connection is closed after the function returns.
func (c *Client) Connect(host string, port int, username, password string) (bool, error) {
	return connect(host, port, username, password, "postgres")
}

// ExecuteQuery connects to Postgres database using given credentials and database name.
// and executes a query on the db.
func (c *Client) ExecuteQuery(host string, port int, username, password, dbName, query string) (string, error) {
	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", username, password, target, dbName)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return "", err
	}

	rows, err := db.Query(query)
	if err != nil {
		return "", err
	}
	resp, err := utils.UnmarshalSQLRows(rows)
	if err != nil {
		return "", err
	}
	return string(resp), nil
}

// ConnectWithDB connects to Postgres database using given credentials and database name.
//
// If connection is successful, it returns true.
// If connection is unsuccessful, it returns false and error.
//
// The connection is closed after the function returns.
func (c *Client) ConnectWithDB(host string, port int, username, password, dbName string) (bool, error) {
	return connect(host, port, username, password, dbName)
}

func connect(host string, port int, username, password, dbName string) (bool, error) {
	if host == "" || port <= 0 {
		return false, fmt.Errorf("invalid host or port")
	}
	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	db := pg.Connect(&pg.Options{
		Addr:     target,
		User:     username,
		Password: password,
		Database: dbName,
	})
	_, err := db.Exec("select 1")
	if err != nil {
		switch true {
		case strings.Contains(err.Error(), "connect: connection refused"):
			fallthrough
		case strings.Contains(err.Error(), "no pg_hba.conf entry for host"):
			fallthrough
		case strings.Contains(err.Error(), "network unreachable"):
			fallthrough
		case strings.Contains(err.Error(), "reset"):
			fallthrough
		case strings.Contains(err.Error(), "i/o timeout"):
			return false, err
		}
		return false, nil
	}
	return true, nil
}
