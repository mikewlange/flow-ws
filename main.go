package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"gopkg.in/gorp.v1"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
)

var webSocketConnections = make(map[*WebSocketConnection]bool)

var debugMode, _ = strconv.ParseBool(GetConfigVar("debugMode"))

// Configure the upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func initializeDatabase() *gorp.DbMap {
	db, err := sql.Open(GetConfigVar("gorpDatabaseType"), GetConfigVar("gorpDatabaseUri"))
	if err != nil {
		log.Printf("err %v", err)
	}
	// @todo need to define dialect basted on GORP Database type
	dbmap := &gorp.DbMap{Db: db, Dialect: gorp.SqliteDialect{}}
	return dbmap
}

type ControllerStatus struct {
	ID                       int    `json:"id" db:"id"`
	LastConnectTimestamp     string `json:"last_connect_timestamp" db:"last_connect_timestamp"`
	ClientVersion            string `json:"client_version" db:"client_version"`
	WebSocketConnected       bool   `json:"web_socket_connected" db:"web_socket_connected"`
	LastWatchdogTimestamp    string `json:"last_watchdog_timestamp" db:"last_watchdog_timestamp"`
	WatchdogNotificationSend bool   `json:"watchdog_notification_sent" db:"watchdog_notification_sent"`
	Attributes               string `json:"attributes" db:"attributes"`
}

type Resource struct {
	ID             int    `json:"id" db:"id"`
	LastRevisionID int    `json:"last_revision_id" db:"last_revision_id"`
	OrganizationID int    `json:"organization_id" db:"organization_id"`
	ParentID       int    `json:"parent_id" db:"parent_id"`
	Name           string `json:"name" db:"name"`
	Deleted        bool   `json:"deleted" db:"deleted"`
}
type OrganizationUser struct {
	ID             int  `json:"id" db:"id"`
	OrganizationID int  `json:"organization_id" db:"organization_id"`
	UserID         int  `json:"user_id" db:"user_id"`
	IsAdmin        bool `json:"is_admin" db:"is_admin"`
}

// Message Define our message object
type Message struct {
	ID                 int             `json:"id" db:"id"`
	Timestamp          string          `json:"timestamp" db:"timestamp"`
	SenderControllerID int             `json:"sender_controller_id" db:"sender_controller_id"` //# the ID of the controller that created the message (if it was not created by a human/browser)
	SenderUserID       int             `json:"sender_user_id" db:"sender_user_id"`
	FolderID           int             `json:"folder_id" db:"folder_id"`
	Type               string          `json:"type" db:"type"`
	Parameters         json.RawMessage `json:"parameters" db:"parameters"`
	Folder             string          `json:"folder" db:"folder"`
	Attributes         sql.NullString  `json:"attributes" db:"attributes"`
}

type MessageParameters struct {
	AuthCode      string                          `json:"authCode"`
	Name          string                          `json:"name"`
	Folder        string                          `json:"folder"`
	FolderID      string                          `json:"folder_id"`
	Subscriptions []MessageParametersSubscription `json:"subscriptions"`
}

type MessageParametersSubscription struct {
	Folder   string `json:"folder"`
	FolderID string `json:"folder_id"`
}

type Key struct {
	ID                   int           `json:"id" db:"id"`
	OrganizationID       string        `json:"organization_id" db:"organization_id"`
	CreationUserID       string        `json:"creation_user_id" db:"creation_user_id"`
	RevocationUserID     sql.NullInt64 `json:"revocation_user_id" db:"revocation_user_id"`
	EmailAddress         string        `json:"email_address" db:"email_address"`
	AccessAsUserID       int           `json:"access_as_user_id" db:"access_as_user_id"`
	AccessAsControllerID int           `json:"access_as_controller_id" db:"access_as_controller_id"`
	CreationTimestamp    string        `json:"creation_timestamp" db:"creation_timestamp"`
	RevocationTimestamp  NullTime      `json:"revocation_timestamp" db:"revocation_timestamp"`
	KeyPart              string        `json:"key_part" db:"key_part"`
	KeyHash              string        `json:"key_hash" db:"key_hash"`
	KeyStorage           string        `json:"key_storage" db:"key_storage"`
	KeyStorageNonce      string        `json:"key_storage_nonce" db:"key_storage_nonce"`
}

type WebSocketConnection struct {
	WS            *websocket.Conn `json:"ws"`
	UserID        int             `json:"id"`
	ControllerID  int             `json:"controller_id"`
	AuthMethod    string          `json:"auth_method"`
	Connected     int             `json:"connected"`
	Subscriptions map[string]bool `json:"subscription"`
}

func (wsConn *WebSocketConnection) Init(r *http.Request, ws *websocket.Conn) {
	// @todo populate appropriately from authenticated user data
	wsConn.WS = ws
	// wsConn.UserID = 4

	// Authentication - either key based via basic http auth, or session based with user_id in session cookie
	basicAuthUsername, basicAuthPassword, basicAuthOk := r.BasicAuth()
	sessionPayloadMap := getSessionCookieFromRequest(r)
	if basicAuthOk == true {
		key := findKey(basicAuthPassword) // key is provided as HTTP basic auth password
		if (Key{} == key) {
			log.Printf("key not found")
			return
		}
		wsConn.ControllerID = key.AccessAsControllerID
		wsConn.UserID = key.AccessAsUserID
		wsConn.AuthMethod = "key"
		if wsConn.ControllerID > 0 {
			dbmap := initializeDatabase()
			defer dbmap.Db.Close()
			var controllerStatus ControllerStatus
			err := dbmap.SelectOne(&controllerStatus, "select * from controller_status where id=:controller_status_id", map[string]interface{}{
				"controller_status_id": wsConn.ControllerID,
			})
			if err != nil {
				log.Printf("warning: unable to find controller status record: %v", err)
			}
			controllerStatus.LastConnectTimestamp = time.Now().Format(time.RFC850)
			controllerStatus.ClientVersion = basicAuthUsername
			_, err = dbmap.Update(&controllerStatus)
			if err != nil {
				log.Printf("warning: unable to update controller status record: %v", err)
			}
		}

	} else if userID, ok := sessionPayloadMap["user_id"].(int); ok {
		wsConn.UserID = userID
		wsConn.AuthMethod = "user"
	}
}

func (wsConn *WebSocketConnection) hasAccess(FolderID string) bool {
	access := false
	if wsConn.ControllerID > 0 {
		access = (string(wsConn.ControllerID) == FolderID)
	} else {
		dbmap := initializeDatabase()
		defer dbmap.Db.Close()

		resource := Resource{}
		organizationUser := OrganizationUser{}
		err := dbmap.SelectOne(&resource, "select * from resource where id=:folder_id AND deleted = false", map[string]interface{}{
			"folder_id": FolderID,
		})

		err = dbmap.SelectOne(&organizationUser, "select * from organization_users where user_id=:user_id AND organization_id=:org_id", map[string]interface{}{
			"user_id": wsConn.UserID,
			"org_id":  resource.OrganizationID,
		})
		if err == nil {
			access = true
		}
	}
	return access
}

// Scan - Implement the database/sql scanner interface
func (msg *Message) Scan(value interface{}) error {
	log.Printf("%v", value)
	return nil
}

func main() {
	// // Configure websocket route
	http.HandleFunc("/api/v1/websocket", manageWebSocket)

	go setUpSocketSender()

	listenPort := GetConfigVar("listenPort")
	// Start the server on localhost port 8000 and log any errors
	log.Println("http server started on", listenPort)
	err := http.ListenAndServe(listenPort, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
