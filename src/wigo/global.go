package wigo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"container/list"
	"errors"
	"github.com/docopt/docopt-go"
	"github.com/fatih/color"
	"github.com/lann/squirrel"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nu7hatch/gouuid"
	"github.com/root-gg/gopentsdb"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"strings"
)

// Static global object
var LocalWigo *Wigo

type Wigo struct {
	Uuid    string
	Version string
	IsAlive bool

	GlobalStatus  int
	GlobalMessage string

	LocalHost   *Host
	RemoteWigos map[string]*Wigo

	Hostname       string
	config         *Config
	locker         *sync.RWMutex
	logfilehandle  *os.File
	gopentsdb      *gopentsdb.OpenTsdb
	disabledProbes *list.List
	uuidObj        *uuid.UUID
	sqlLiteConn    *sql.DB
	sqlLiteLock    *sync.Mutex

	push       *PushServer
	LastUpdate int64
}

var Version = "##VERSION##"

func NewWigo(config *Config) (this *Wigo, err error) {
	this = new(Wigo)

	this.config = config

	this.IsAlive = true
	this.Version = Version
	this.GlobalStatus = 100
	this.GlobalMessage = "OK"

	// Load uuid
	if _, err = os.Stat(this.config.Global.UuidFile); err == nil {
		if uuidBytes, err := ioutil.ReadFile(this.config.Global.UuidFile); err == nil {
			this.Uuid = string(uuidBytes)
		} else {
			log.Fatalf("Unable to read uuid file : %s", err)
		}
	} else {
		// Create UUID
		this.uuidObj, err = uuid.NewV4()
		if err != nil {
			log.Fatalf("Failed to create uuid : %s", err)
		} else {
			this.Uuid = this.uuidObj.String()
			log.Printf("Wigo uuid is : %s", this.Uuid)
		}

		// Save UUID
		uuidFile, err := os.OpenFile(this.config.Global.UuidFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err == nil {
			uuidFile.Write([]byte(this.Uuid))
			uuidFile.Close()
		} else {
			log.Fatalf("Failed to create uuid file : %s", err)
		}
	}

	// Get hostname
	if this.config.Global.Hostname == "" {
		// Get hostname
		localHostname, err := os.Hostname()
		if err == nil {
			this.config.Global.Hostname = localHostname
		} else {
			log.Println("Couldn't get hostname for local machine, using localhost")
			this.config.Global.Hostname = "localhost"
		}
	}

	// Get groupname
	if this.config.Global.Group == "" {
		this.config.Global.Group = "local"
	}

	// Init LocalHost
	this.Hostname = this.config.Global.Hostname
	this.LocalHost = NewHost()
	this.LocalHost.Name = this.config.Global.Hostname
	this.LocalHost.Group = this.config.Global.Group
	this.LocalHost.parentWigo = this

	// Init RemoteWigos list
	this.RemoteWigos = make(map[string]*Wigo)

	// Private vars
	this.locker = new(sync.RWMutex)
	this.disabledProbes = new(list.List)

	return
}

func InitWigo() (err error) {
	// Clean temporary files
	removeFunc := func(path string, f os.FileInfo, err error) (e error) {
		if strings.HasSuffix(path, ".wigo") {
			os.Remove(path)
		}
		return
	}

	filepath.Walk("/tmp", removeFunc)

	// Args
	usage := `wigo

Usage:
	wigo
	wigo [options]

Options:
	-h 	--help
	-v 	--version
	-c, --config CONFIG		Specify config file
`

	// Parse args
	configFile := "/etc/wigo/wigo.conf"

	arguments, _ := docopt.Parse(usage, nil, true, Version, false)

	for key, value := range arguments {
		if _, ok := value.(string); ok {
			if key == "--config" {
				configFile = value.(string)
			}
		}
	}

	LocalWigo, err = NewWigo(NewConfig(configFile))
	if err != nil {
		return err
	}

	config := LocalWigo.GetConfig()

	// Log file
	LocalWigo.InitOrReloadLogger()

	// Test probes directory
	_, err = os.Stat(config.Global.ProbesDirectory)
	if err != nil {
		return err
	}

	// Init channels
	InitChannels()

	// Rpc
	if LocalWigo.config.PushServer.Enabled {
		runtime.GOMAXPROCS(runtime.NumCPU())
		LocalWigo.push = NewPushServer(LocalWigo.config.PushServer)
	}

	// OpenTSDB
	if LocalWigo.config.OpenTSDB.Enabled {
		if LocalWigo.gopentsdb, err = gopentsdb.NewOpenTsdb(config.OpenTSDB.Address, config.OpenTSDB.SslEnabled, config.OpenTSDB.Deduplication, config.OpenTSDB.BufferSize); err != nil {
			log.Fatal(err)
		}
		gopentsdb.Verbose(config.Global.Debug)
	}

	// SqlLite
	LocalWigo.sqlLiteLock = new(sync.Mutex)
	LocalWigo.sqlLiteConn, err = sql.Open("sqlite3", LocalWigo.config.Global.Database)
	if err != nil {
		log.Fatalf("Fail to init sqllite database %s : %s", LocalWigo.config.Global.Database, err)
	}

	sqlStmt := `
    CREATE TABLE IF NOT EXISTS logs (id integer not null primary key, date timestamp, level int, grp text, host text, probe text, message text) ;
    `
	_, err = LocalWigo.sqlLiteConn.Exec(sqlStmt)
	if err != nil {
		log.Fatalf("Fail to create table in sqlite database : %s\n", err)
	}

	// Launch cleaning routing
	go func() {
		for {
			ts := time.Now().Unix() - 86400*30
			sqlStmt := `DELETE FROM logs WHERE date < ?;`

			LocalWigo.sqlLiteLock.Lock()
			_, err = LocalWigo.sqlLiteConn.Exec(sqlStmt, ts)
			if err != nil {
				log.Fatalf("Fail to clean logs in database : %s\n", err)
			}
			LocalWigo.sqlLiteLock.Unlock()

			time.Sleep(time.Hour)
		}
	}()

	// UP / DOWN
	go func() {
		for {
			now := time.Now().Unix()
			for _, host := range LocalWigo.RemoteWigos {
				if host.LastUpdate < now-int64(config.Global.AliveTimeout) {
					if host.IsAlive {
						host.Down()
					}
				} else {
					if !host.IsAlive {
						host.Up()
					}
				}
			}

			time.Sleep(time.Second)
		}
	}()

	return nil
}

// Factory

func GetLocalWigo() *Wigo {
	return LocalWigo
}

// Constructors

func NewWigoFromJson(ba []byte, checkRemotesDepth int) (this *Wigo, e error) {

	this = new(Wigo)

	err := json.Unmarshal(ba, this)
	if err != nil {
		return nil, err
	}

	if checkRemotesDepth != 0 {
		this = this.EraseRemoteWigos(checkRemotesDepth)
	}

	this.SetParentHostsInProbes()
	this.LocalHost.SetParentWigo(this)

	// For backward compatibility
	if this.Hostname == "" {
		this.Hostname = this.LocalHost.Name
	}

	return
}

// Status setters
func (this *Wigo) Down() {
	this.GlobalStatus = 999
	this.LocalHost.Status = 999
	this.GlobalMessage = "DOWN"
	this.IsAlive = false

	// Send notification
	SendNotification(NewNotificationFromMessage(fmt.Sprintf("Host %s DOWN", this.Hostname)))

	// Add a log
	LocalWigo.AddLog(this, CRITICAL, fmt.Sprintf("Wigo %s DOWN", this.Hostname))
}

func (this *Wigo) Up() {
	this.GlobalMessage = "UP"
	this.IsAlive = true

	// Send notification
	SendNotification(NewNotificationFromMessage(fmt.Sprintf("Host %s UP", this.Hostname)))

	// Add a log
	LocalWigo.AddLog(this, INFO, fmt.Sprintf("Wigo %s UP", this.Hostname))
}

// Recompute statuses
func (this *Wigo) RecomputeGlobalStatus() {

	this.GlobalStatus = 0

	// Local probes
	for probeName := range this.LocalHost.Probes {
		if this.LocalHost.Probes[probeName].Status > this.GlobalStatus {
			this.GlobalStatus = this.LocalHost.Probes[probeName].Status
		}
	}

	return
}

// Getters
func (this *Wigo) GetLocalHost() *Host {
	return this.LocalHost
}

func (this *Wigo) GetConfig() *Config {
	return this.config
}

func (this *Wigo) GetHostname() string {
	return this.Hostname
}

func (this *Wigo) GetOpenTsdb() *gopentsdb.OpenTsdb {
	return this.gopentsdb
}

func (this *Wigo) Deduplicate(remoteWigo *Wigo) (err error) {
	for uuid, wigo := range remoteWigo.RemoteWigos {
		if uuid != wigo.Uuid {
			return errors.New(fmt.Sprintf("Remote wigo %s uuid mismatch ...", wigo.GetHostname()))
		}
		if wigo.Uuid != "" && this.Uuid == wigo.Uuid {
			log.Printf("Try to add a remote wigo %s with same uuid as me, Discarding.", wigo.GetHostname())
			delete(remoteWigo.RemoteWigos, uuid)
		}
		if _, ok := this.RemoteWigos[uuid]; ok {
			log.Printf("Found a duplicate wigo %s, Discarding.", wigo.GetHostname())
			delete(remoteWigo.RemoteWigos, uuid)
		}
		if err := this.Deduplicate(wigo); err != nil {
			return err
		}
	}
	return
}

func (this *Wigo) AddOrUpdateRemoteWigo(remoteWigo *Wigo) {

	this.Lock()
	defer this.Unlock()

	// Test if remote is not me :D
	if remoteWigo.Uuid != "" && this.Uuid == remoteWigo.Uuid {
		log.Printf("Try to add a remote wigo %s with same uuid as me.. Discarding..", remoteWigo.GetHostname())
		return
	}
	if err := this.Deduplicate(remoteWigo); err != nil {
		log.Println(err)
		return
	}

	if oldWigo, ok := this.RemoteWigos[remoteWigo.Uuid]; ok {
		if !oldWigo.IsAlive {
			remoteWigo.IsAlive = false
		}
		this.CompareTwoWigosAndRaiseNotifications(oldWigo, remoteWigo)
	}

	this.RemoteWigos[remoteWigo.Uuid] = remoteWigo
	this.RemoteWigos[remoteWigo.Uuid].LastUpdate = time.Now().Unix()
	this.RecomputeGlobalStatus()
}

func (this *Wigo) CompareTwoWigosAndRaiseNotifications(oldWigo *Wigo, newWigo *Wigo) {
	// Detect changes and deleted probes
	if oldWigo.LocalHost != nil {

		for probeName := range oldWigo.LocalHost.Probes {
			oldProbe := oldWigo.LocalHost.Probes[probeName]

			if probeWhichStillExistInNew, ok := newWigo.LocalHost.Probes[probeName]; ok {

				// Probe still exist in new
				newWigo.LocalHost.SetParentWigo(newWigo)

				// Graph
				probeWhichStillExistInNew.GraphMetrics()

				// Status has changed ? -> Notification
				if oldProbe.Status != probeWhichStillExistInNew.Status {
					NewNotificationProbe(oldProbe, probeWhichStillExistInNew)
				}
			} else {

				// Prob disappeard !
				if newWigo.IsAlive {
					NewNotificationProbe(oldProbe, nil)
				}
			}
		}
	}

	// Detect new probes (only if new wigo is up)
	if newWigo.IsAlive && oldWigo.IsAlive {
		for probeName := range newWigo.LocalHost.Probes {
			if _, ok := oldWigo.LocalHost.Probes[probeName]; !ok {
				NewNotificationProbe(nil, newWigo.LocalHost.Probes[probeName])
			}
		}
	}

	// Remote Wigos
	for wigoName := range oldWigo.RemoteWigos {

		oldWigo := oldWigo.RemoteWigos[wigoName]

		if wigoStillExistInNew, ok := newWigo.RemoteWigos[wigoName]; ok {
			// Recursion
			this.CompareTwoWigosAndRaiseNotifications(oldWigo, wigoStillExistInNew)
		}
	}
}

func (this *Wigo) SetParentHostsInProbes() {
	for localProbeName := range this.GetLocalHost().Probes {
		this.LocalHost.Probes[localProbeName].SetHost(this.LocalHost)
	}

	for remoteWigo := range this.RemoteWigos {
		this.RemoteWigos[remoteWigo].SetParentHostsInProbes()
	}
}

// Reloads

func (this *Wigo) InitOrReloadLogger() (err error) {
	if this.logfilehandle != nil {
		err = this.logfilehandle.Close()
		if err != nil {
			return err
		}
	}

	f, err := os.OpenFile(LocalWigo.GetConfig().Global.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Fail to open logfile %s : %s\n", LocalWigo.GetConfig().Global.LogFile, err)
		return err
	} else {
		LocalWigo.logfilehandle = f
		writer := io.MultiWriter(os.Stdout, f)

		log.SetOutput(writer)
		log.SetPrefix(LocalWigo.GetLocalHost().Name + " ")
	}

	return nil
}

// Locks
func (this *Wigo) Lock() {
	this.locker.Lock()
}

func (this *Wigo) Unlock() {
	this.locker.Unlock()
}

// Logs
func (this *Wigo) AddLog(ressource interface{}, level uint8, message string) (err error) {

	// Instanciate log
	newLog := NewLog(level, message)

	if existingLog, ok := ressource.(*Log); ok {
		newLog = existingLog
	} else {
		newLog = NewLog(level, message)
	}
	log.Printf("%s", message)

	// Type assertion on ressource
	switch v := ressource.(type) {
	case *ProbeResult:
		newLog.Probe = v.Name
		newLog.Level = NOTICE
		newLog.Host = v.GetHost().GetParentWigo().GetHostname()
		newLog.Group = v.GetHost().Group

		// Level
		if v.Status > 100 && v.Status < 200 {
			newLog.Level = INFO
		} else if v.Status >= 200 && v.Status < 300 {
			newLog.Level = WARNING
		} else if v.Status >= 300 && v.Status < 500 {
			newLog.Level = CRITICAL
		} else if v.Status >= 500 {
			newLog.Level = ERROR
		}

	case *Wigo:
		newLog.Host = v.GetHostname()
		newLog.Group = v.GetLocalHost().Group
		newLog.Level = NOTICE

		// Level
		if v.GlobalStatus > 100 && v.GlobalStatus < 200 {
			newLog.Level = INFO
		} else if v.GlobalStatus >= 200 && v.GlobalStatus < 300 {
			newLog.Level = WARNING
		} else if v.GlobalStatus >= 300 && v.GlobalStatus < 500 {
			newLog.Level = CRITICAL
		} else if v.GlobalStatus >= 500 {
			newLog.Level = ERROR
		}

	case string:
		newLog.Group = v
	}

	go newLog.Persist()

	return nil
}

func (this *Wigo) SearchLogs(probe string, hostname string, group string, limit uint64, offset uint64) []*Log {

	// Lock
	LocalWigo.sqlLiteLock.Lock()
	defer LocalWigo.sqlLiteLock.Unlock()

	// Construct SQL Query
	logs := make([]*Log, 0)
	logsQuery := squirrel.Select("date,level,grp,host,probe,message").From("logs")

	if probe != "" {
		logsQuery = logsQuery.Where(squirrel.Eq{"probe": probe})
	}
	if hostname != "" {
		logsQuery = logsQuery.Where(squirrel.Eq{"host": hostname})
	}
	if group != "" {
		logsQuery = logsQuery.Where(squirrel.Eq{"grp": group})
	}

	// Index && Offset
	logsQuery = logsQuery.OrderBy("id DESC")
	logsQuery = logsQuery.Limit(limit)
	logsQuery = logsQuery.Offset(offset)

	// Execute
	rows, err := logsQuery.RunWith(LocalWigo.sqlLiteConn).Query()
	if err != nil {
		log.Printf("Fail to exec query to fetch logs : %s", err)
		return logs
	}

	// Instanciate
	for rows.Next() {
		l := new(Log)
		t := new(time.Time)

		if err := rows.Scan(&t, &l.Level, &l.Group, &l.Host, &l.Probe, &l.Message); err != nil {
			return logs
		}

		l.Timestamp = t.Unix()
		l.Date = t.Format(dateLayout)

		logs = append(logs, l)
	}

	return logs
}

// Serialize
func (this *Wigo) ToJsonString() (string, error) {

	// Send json to socket channel
	j, e := json.Marshal(this)
	if e != nil {
		return "", e
	}

	return string(j), nil
}

// Disabled probes
func (this *Wigo) GetDisabledProbes() *list.List {
	return this.disabledProbes
}
func (this *Wigo) DisableProbe(probeName string) {
	alreadyDisabled := false

	if probeName == "" {
		return
	}

	// Check if not already disabled
	for e := this.disabledProbes.Front(); e != nil; e = e.Next() {
		if p, ok := e.Value.(string); ok {
			if p == probeName {
				alreadyDisabled = true
			}
		}
	}

	if !alreadyDisabled {
		this.disabledProbes.PushBack(probeName)
	}

	return
}
func (this *Wigo) IsProbeDisabled(probeName string) bool {

	for e := this.disabledProbes.Front(); e != nil; e = e.Next() {
		if p, ok := e.Value.(string); ok {
			if p == probeName {
				return true
			}
		}
	}

	return false
}

// Summaries

func (this *Wigo) GenerateSummary(showOnlyErrors bool) (summary string) {

	red := color.New(color.FgRed).SprintfFunc()
	yellow := color.New(color.FgYellow).SprintfFunc()

	summary += fmt.Sprintf("%s running on %s \n", this.Version, this.LocalHost.Name)
	summary += fmt.Sprintf("Local Status 	: %d\n", this.LocalHost.Status)
	summary += fmt.Sprintf("Global Status	: %d\n\n", this.GlobalStatus)

	if this.LocalHost.Status != 100 || !showOnlyErrors {
		summary += "Local probes : \n\n"

		for probeName := range this.LocalHost.Probes {
			if this.LocalHost.Probes[probeName].Status > 100 && this.LocalHost.Probes[probeName].Status < 300 {
				summary += yellow("\t%-25s : %d  %s\n", this.LocalHost.Probes[probeName].Name, this.LocalHost.Probes[probeName].Status, strings.Replace(this.LocalHost.Probes[probeName].Message, "%", "%%", -1))
			} else if this.LocalHost.Probes[probeName].Status >= 300 {
				summary += red("\t%-25s : %d  %s\n", this.LocalHost.Probes[probeName].Name, this.LocalHost.Probes[probeName].Status, strings.Replace(this.LocalHost.Probes[probeName].Message, "%", "%%", -1))
			} else {
				summary += fmt.Sprintf("\t%-25s : %d  %s\n", this.LocalHost.Probes[probeName].Name, this.LocalHost.Probes[probeName].Status, strings.Replace(this.LocalHost.Probes[probeName].Message, "%", "%%", -1))
			}
		}

		summary += "\n"
	}

	if this.GlobalStatus >= 200 && len(this.RemoteWigos) > 0 {
		summary += "Remote Wigos : \n\n"
	}

	summary += this.GenerateRemoteWigosSummary(0, showOnlyErrors, this.Version)

	return
}

func (this *Wigo) GenerateRemoteWigosSummary(level int, showOnlyErrors bool, version string) (summary string) {

	red := color.New(color.FgRed).SprintfFunc()
	yellow := color.New(color.FgYellow).SprintfFunc()

	for remoteWigo := range this.RemoteWigos {

		if showOnlyErrors && this.RemoteWigos[remoteWigo].GlobalStatus < 200 {
			continue
		}

		// Nice align
		tabs := ""
		for i := 0; i <= level; i++ {
			tabs += "\t"
		}

		// Host down ?
		if !this.RemoteWigos[remoteWigo].IsAlive {
			summary += tabs + red(this.RemoteWigos[remoteWigo].GetHostname()+" DOWN : \n")
			summary += tabs + red("\t"+this.RemoteWigos[remoteWigo].GlobalMessage+"\n")

		} else {
			if this.RemoteWigos[remoteWigo].Version != version {
				summary += tabs + this.RemoteWigos[remoteWigo].GetHostname() + " ( " + this.RemoteWigos[remoteWigo].LocalHost.Name + " ) - " + red(this.RemoteWigos[remoteWigo].Version) + ": \n"
			} else {
				summary += tabs + this.RemoteWigos[remoteWigo].GetHostname() + " ( " + this.RemoteWigos[remoteWigo].LocalHost.Name + " ) - " + this.RemoteWigos[remoteWigo].Version + ": \n"
			}
		}

		// Iterate on probes
		for probeName := range this.RemoteWigos[remoteWigo].GetLocalHost().Probes {

			currentProbe := this.RemoteWigos[remoteWigo].GetLocalHost().Probes[probeName]
			summary += tabs

			if currentProbe.Status > 100 && currentProbe.Status < 300 {
				summary += yellow("\t%-25s : %d  %s\n", currentProbe.Name, currentProbe.Status, strings.Replace(currentProbe.Message, "%", "%%", -1))
			} else if currentProbe.Status >= 300 {
				summary += red("\t%-25s : %d  %s\n", currentProbe.Name, currentProbe.Status, strings.Replace(currentProbe.Message, "%", "%%", -1))
			} else {
				summary += fmt.Sprintf("\t%-25s : %d  %s\n", currentProbe.Name, currentProbe.Status, strings.Replace(currentProbe.Message, "%", "%%", -1))
			}
		}

		nextLevel := level + 1
		summary += "\n"
		summary += this.RemoteWigos[remoteWigo].GenerateRemoteWigosSummary(nextLevel, showOnlyErrors, version)
	}

	return
}

func (this *Wigo) FindRemoteWigoByHostname(hostname string) *Wigo {
	var foundWigo *Wigo

	if this.GetHostname() == hostname {
		return this
	}

	for name := range this.RemoteWigos {

		if name == hostname {
			foundWigo = this.RemoteWigos[name]
			return foundWigo
		}

		foundWigo = this.RemoteWigos[name].FindRemoteWigoByHostname(hostname)
		if foundWigo != nil {
			return foundWigo
		}
	}

	return foundWigo
}

func (this *Wigo) FindRemoteWigoByUuid(uuid string) (*Wigo, bool) {
	if wigo, ok := LocalWigo.RemoteWigos[uuid]; ok {
		return wigo, true
	} else {
		for _, w := range this.RemoteWigos {
			w.FindRemoteWigoByUuid(uuid)
		}
	}
	return nil, false
}

func (this *Wigo) ListRemoteWigosNames() []string {
	list := make([]string, 0)

	if this.Uuid == LocalWigo.Uuid {
		list = append(list, this.GetHostname())
	}

	for wigoName := range this.RemoteWigos {
		list = append(list, this.RemoteWigos[wigoName].GetHostname())
		remoteList := this.RemoteWigos[wigoName].ListRemoteWigosNames()
		list = append(list, remoteList...)
	}

	return list
}

func (this *Wigo) ListProbes() []string {
	list := make([]string, 0)

	for probe := range this.LocalHost.Probes {
		list = append(list, probe)
	}

	return list
}

// Erase RemoteWigos if maximum wanted depth is reached

func (this *Wigo) EraseRemoteWigos(depth int) *Wigo {

	depth = depth - 1

	if depth == 0 {
		this.RemoteWigos = make(map[string]*Wigo)
		this.RecomputeGlobalStatus()
		return this
	} else {
		for remoteWigo := range this.RemoteWigos {
			this.RemoteWigos[remoteWigo].EraseRemoteWigos(depth)
			this.RemoteWigos[remoteWigo].RecomputeGlobalStatus()
		}
	}

	return this
}

// Groups

func (this *Wigo) ListGroupsNames() []string {
	list := make([]string, 0)

	if this.Uuid == LocalWigo.Uuid && this.GetLocalHost().Group != "" {
		list = append(list, this.GetLocalHost().Group)
	}

	for wigoName := range this.RemoteWigos {
		group := this.RemoteWigos[wigoName].GetLocalHost().Group

		if !IsStringInArray(group, list) && group != "" {
			list = append(list, this.RemoteWigos[wigoName].GetLocalHost().Group)
		}

		remoteList := this.RemoteWigos[wigoName].ListGroupsNames()

		for i := range remoteList {
			if !IsStringInArray(remoteList[i], list) {
				list = append(list, remoteList[i])
			}
		}
	}

	return list
}

func (this *Wigo) GroupSummary(groupName string) (hs []*HostSummary, status int) {
	hs = make([]*HostSummary, 0)

	status = 0

	if this.GetLocalHost().Group == groupName {
		summary := this.GetLocalHost().GetSummary()
		summary.Name = this.GetHostname()
		summary.Status = this.GlobalStatus
		summary.Message = this.GlobalMessage
		summary.IsAlive = this.IsAlive

		hs = append(hs, summary)

		if this.GetLocalHost().Status > status {
			status = this.GetLocalHost().Status
		}

		if this.GlobalStatus > status {
			status = this.GlobalStatus
		}
	}

	for remoteWigoName := range this.RemoteWigos {
		subSummaries, subStatus := this.RemoteWigos[remoteWigoName].GroupSummary(groupName)

		if len(subSummaries) > 0 {
			hs = append(hs, subSummaries...)
		}

		if subStatus > status {
			status = subStatus
		}
	}

	return hs, status
}
