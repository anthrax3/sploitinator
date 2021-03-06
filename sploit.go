package main

import (
	"bytes"
	"database/sql"
	"fmt"
	auth "github.com/abbot/go-http-auth"
	_ "github.com/lib/pq"
	flag "github.com/ogier/pflag"
	"github.com/op/go-logging"
	"github.com/snarlysodboxer/msfapi"
	"gopkg.in/fsnotify.v1"
	"gopkg.in/robfig/cron.v2"
	"gopkg.in/yaml.v2"
	"html/template"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type msfConfig struct {
	User string
	Pass string
	Host string
	Port string
	URI  string
}

type sploitConfig struct {
	WatchDir     string
	ServicesFile string
	ServeAddress string
	UpdateSpec   string
	StatusSpec   string
	LogFile      string
}

type postgresConfig struct {
	User string
	Pass string
	Host string
	Port string
	DB   string
}

type smtpConfig struct {
	User string
	Pass string
	Host string
	From string
	To   string
}

type config struct {
	Sploit   sploitConfig
	MsfRpc   msfConfig
	Postgres postgresConfig
	SMTP     smtpConfig
}

type module struct {
	Name     string
	Commands []string
	CronSpec string
	Running  bool
}

type host struct {
	Name     string
	Services []struct {
		Name  string
		Ports []int
	}
}

type service struct {
	Name    string
	Modules []module
}

type Daemon struct {
	Hosts         []host
	Services      *[]service
	API           *msfapi.API
	interruptChan chan os.Signal
	notifierChan  chan bool
	errorChan     chan string
	cron          cron.Cron
	internalCron  cron.Cron
	updateRunning bool
	lastUpdate    time.Time
	waitGroup     sync.WaitGroup
	cfg           config
	configFile    *string
	db            sql.DB
	knownVulnIDs  []int
	knownModules  map[string][]string
	scanCount     int
	logWriter     *os.File
	debugMode     *bool
}

type vulnerability struct {
	id         int
	CreatedAt  time.Time
	Address    string
	Name       string
	References string
}

func (daemon *Daemon) SetupLogging() {
	var logFormat = logging.MustStringFormatter(
		"%{color}%{time:15:04:05.000} %{level:.8s} %{shortfunc} ▶ %{message}%{color:reset}",
	)
	var multiWriter io.Writer
	if daemon.cfg.Sploit.LogFile != "" {
		daemon.logWriter = createAndLoadFile(daemon.cfg.Sploit.LogFile)
		multiWriter = io.MultiWriter(os.Stderr, daemon.logWriter)
	} else {
		multiWriter = io.MultiWriter(os.Stderr)
	}
	logBackend := logging.NewLogBackend(multiWriter, "", 0)
	logBackendFormatter := logging.NewBackendFormatter(logBackend, logFormat)
	logBackendLeveled := logging.AddModuleLevel(logBackendFormatter)
	logging.SetBackend(logBackendLeveled)
	if *daemon.debugMode {
		logBackendLeveled.SetLevel(logging.DEBUG, "sploit")
	} else {
		logBackendLeveled.SetLevel(logging.INFO, "sploit")
	}
}

func (daemon *Daemon) LoadFlags() {
	daemon.configFile = flag.StringP("config-file", "c", "sploit.yml", "File to read Sploit settings")
	daemon.debugMode = flag.Bool("debug", false, "Enable debug logging")
	flag.Usage = func() {
		fmt.Printf("Usage:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if !(flag.NFlag() == 1 || flag.NFlag() == 2) {
		flag.Usage()
		os.Exit(1)
	}
	_, err := os.Stat(*daemon.configFile)
	fileExists := !os.IsNotExist(err)
	if !fileExists {
		log.Fatalf("File %s not found!", *daemon.configFile)
	}
}

// supply a mechanism for stopping
func (daemon *Daemon) CreateInterruptChannel() {
	daemon.interruptChan = make(chan os.Signal, 1)
	signal.Notify(daemon.interruptChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		for _ = range daemon.interruptChan {
			log.Info("Closing....")
			daemon.cron.Stop()                                  // Stop the scheduler (does not stop any jobs already running).
			err := daemon.API.AuthTokenRemove(daemon.API.Token) // essentially logout
			if err != nil {
				message := fmt.Sprintf("Error removing Auth token:\n%v", err)
				log.Critical(message)
				daemon.errorChan <- message
			}
			log.Debug("Removed auth token %v", daemon.API.Token)
			defer daemon.db.Close()
			defer daemon.logWriter.Close()
			daemon.waitGroup.Done()
		}
	}()
}

// read sploit.yml and set settings
func (daemon *Daemon) LoadSploitYaml() {
	contents, err := ioutil.ReadFile(*daemon.configFile)
	if err != nil {
		log.Fatal(err)
	}
	cfg := config{}
	err = yaml.Unmarshal([]byte(contents), &cfg)
	if err != nil {
		log.Fatal(err)
	}
	daemon.cfg = cfg
	log.Info("Successfully loaded Sploit yaml file")
}

// read services.yml and map service names to Metasploit modules
func (daemon *Daemon) LoadServicesYaml() {
	contents, err := ioutil.ReadFile(daemon.cfg.Sploit.ServicesFile)
	if err != nil {
		message := fmt.Sprintf("Error loading services.yml YAML file:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	services := []service{}
	err = yaml.Unmarshal([]byte(contents), &services)
	if err != nil {
		message := fmt.Sprintf("Error unmarshaling YAML file:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	daemon.Services = &services // overwrite old
	log.Info("Successfully loaded services.yml file")
	log.Debug("Services/Modules config is: %v", services)
}

// read host.yml files from host.d into daemon.Hosts
func (daemon *Daemon) LoadHostYamls() {
	files, err := ioutil.ReadDir(daemon.cfg.Sploit.WatchDir)
	if err != nil {
		message := fmt.Sprintf("Error reading host.d directory:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	hosts := []host{}
	for _, file := range files {
		if !file.IsDir() {
			regex := regexp.MustCompilePOSIX(".*.yml$")
			if regex.MatchString(file.Name()) {
				contents, err := ioutil.ReadFile(fmt.Sprintf("%s/%s", daemon.cfg.Sploit.WatchDir, file.Name()))
				if err != nil {
					message := fmt.Sprintf("Error loading YAML file:\n%v", err)
					log.Critical(message)
					daemon.errorChan <- message
					return
				}
				host := host{}
				err = yaml.Unmarshal([]byte(contents), &host)
				if err != nil {
					message := fmt.Sprintf("Error unmarshaling YAML file:\n%v", err)
					log.Critical(message)
					daemon.errorChan <- message
					return
				}
				hosts = append(hosts, host)
			}
		}
	}
	daemon.Hosts = hosts // overwrite old
	log.Info("Successfully loaded hosts yaml files")
	log.Debug("Host configs are: %v", hosts)
}

func (daemon *Daemon) SetupAPIToken() {

	// TODO create an interrupt here
	open := false
	for open == false {
		address := fmt.Sprintf("%s:%s", daemon.cfg.MsfRpc.Host, daemon.cfg.MsfRpc.Port)
		conn, err := net.Listen("tcp", address)
		if err == nil {
			conn.Close()
			log.Debug(fmt.Sprintf("Waiting 3 seconds for msfrpcd to take port %s", daemon.cfg.MsfRpc.Port))
			time.Sleep(3 * time.Second)
		} else {
			log.Debug(fmt.Sprintf("%s", err))
			log.Debug(fmt.Sprintf("msfrpcd has taken port %s, continuing", daemon.cfg.MsfRpc.Port))
			open = true
		}
	}

	tempToken, err := daemon.API.AuthLogin(daemon.cfg.MsfRpc.User, daemon.cfg.MsfRpc.Pass)
	if err != nil {
		log.Fatalf("Error with AuthLogin: %s", err)
	}
	log.Debug("Logged into the MSF API, got temporary auth token: %s", tempToken)
	daemon.API.Token = tempToken

	permToken := strings.Replace(tempToken, "TEMP", "PERM", -1)
	err = daemon.API.AuthTokenAdd(permToken)
	if err != nil {
		log.Fatalf("Error with AuthTokenAdd: %s", err)
	}
	log.Debug("Added permanent auth token: %s", permToken)
	daemon.API.Token = permToken

	err = daemon.API.AuthTokenRemove(tempToken)
	if err != nil {
		log.Fatalf("Error with AuthTokenRemove: %s", err)
	}
	log.Debug("Removed temporary auth token: %v", tempToken)

	tokens, err := daemon.API.AuthTokenList()
	if err != nil {
		log.Fatalf("Error with AuthTokenList: %s", err)
	}
	log.Debug("Current Token list: %v", tokens)
}

func (daemon *Daemon) OpenDBConnection() {
	cfg := daemon.cfg.Postgres
	credentials := fmt.Sprintf(
		"user=%s password=%s host=%s port=%s dbname=%s sslmode=disable",
		cfg.User, cfg.Pass, cfg.Host, cfg.Port, cfg.DB,
	)
	log.Debug(fmt.Sprintf("Using PostgreSQL login credentials \"%s\"", credentials))
	db, err := sql.Open("postgres", credentials)
	if err != nil {
		log.Fatal(err)
	}
	daemon.db = *db
	err = daemon.db.Ping()
	if err != nil {
		log.Fatal(err)
	}
	log.Info("Successfully opened a database connection")
}

// setup cron entries which send api calls to msfrpcd
func (daemon *Daemon) CreateCronEntries() {
	for _, daemonService := range *daemon.Services {
		for _, module := range daemonService.Modules {
			log.Info("Creating a cron entry for: %v", module.Name)
			module.Running = false
			_, err := daemon.cron.AddFunc(module.CronSpec, func() {
				daemon.runModuleAgainstEachHostPort(daemonService.Name, &module)
			})
			if err != nil {
				message := fmt.Sprintf("Error adding cron entry:\n%v", err)
				log.Critical(message)
				daemon.errorChan <- message
				return
			}
			daemon.waitGroup.Add(1)
		}
	}
}

// to be run by cron
func (daemon *Daemon) runModuleAgainstEachHostPort(serviceName string, module *module) {
	log.Info("Triggered cron entry for module %s", module.Name)
	if module.Running {
		log.Warning("Module %s is already running, not running again.", module.Name)
		return
	}
	startTime := time.Now()
	module.Running = true
	for _, host := range daemon.Hosts {
		for _, hostService := range host.Services {
			if hostService.Name == serviceName {
				for _, port := range hostService.Ports {
					var commands []string
					for _, command := range module.Commands {
						cmd := strings.Replace(command, "SPLOITHOSTNAME", host.Name, -1)
						cmd = strings.Replace(cmd, "SPLOITHOSTPORT", strconv.Itoa(port), -1)
						cmd = fmt.Sprintf("%s\n", cmd)
						commands = append(commands, cmd)
					}

					log.Info("Initiating '%s' to run against port '%d' on '%v'.",
						module.Name, port, host.Name)
					log.Debug("Module details: %v", *module)
					log.Debug("Commands that will be run: %v", commands)
					_, err := daemon.createConsoleAndRun(commands)
					if err != nil {
						message := fmt.Sprintf("Error running commands in console:\n%v", err)
						log.Critical(message)
						daemon.errorChan <- message
						return
					}
				}
			}
		}
	}
	log.Info("%v took %v to run", module.Name, time.Since(startTime))
	module.Running = false
	daemon.scanCount = daemon.scanCount + 1
	daemon.notifierChan <- true
}

func (daemon *Daemon) CreateNotifier() {
	daemon.notifierChan = make(chan bool, 10)
	go func() {
		for range daemon.notifierChan {
			daemon.recordAndNotify()
		}
	}()
}

// check for vulns in the database with an sql statement
func (daemon *Daemon) selectVulns() (*[]vulnerability, error) {
	vulnerabilities := []vulnerability{}
	query := strings.Join([]string{
		"SELECT vulns.id,vulns.created_at,hosts.address,vulns.name,array_agg(refs.name) AS references",
		"FROM vulns,hosts,vulns_refs,refs",
		"WHERE vulns.host_id = hosts.id AND refs.id = vulns_refs.ref_id AND vulns_refs.vuln_id = vulns.id",
		"GROUP BY vulns.id,vulns.created_at,hosts.address,vulns.name;",
	}, " ")
	vulns, err := daemon.db.Query(query)
	if err != nil {
		return &vulnerabilities, err
	}
	defer vulns.Close()
	for vulns.Next() {
		var vuln = vulnerability{}
		err = vulns.Scan(&vuln.id, &vuln.CreatedAt, &vuln.Address, &vuln.Name, &vuln.References)
		if err != nil {
			return &vulnerabilities, err
		}
		vuln.References = strings.Replace(vuln.References, ",", " ", -1)
		vuln.References = strings.Replace(vuln.References, "-http", " http", 1)
		log.Debug("Vulnerability found:\n%v %v %v %v",
			vuln.CreatedAt, vuln.Address, vuln.Name, vuln.References)
		vulnerabilities = append(vulnerabilities, vuln)
	}
	return &vulnerabilities, nil
}

// record vulns database IDs and send emails when new vulns are found
func (daemon *Daemon) recordAndNotify() {
	vulns, err := daemon.selectVulns()
	if err != nil {
		message := fmt.Sprintf("Error selecting vulns from the database:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	for _, vuln := range *vulns {
		known := false
		for _, vulnID := range daemon.knownVulnIDs {
			if vulnID == vuln.id {
				known = true
			}
		}
		if known {
			log.Debug("Vulnerabilty %v is already known, not notifying", vuln.id)
		} else {
			var subject = fmt.Sprintf("New Vulnerability found on %s", vuln.Address)
			var message = fmt.Sprintf("Found the folowing Vulnerability on %s\n\n%s %s %s %s",
				vuln.Address, vuln.CreatedAt, vuln.Address, vuln.Name, vuln.References,
			)
			daemon.sendEmail(&subject, &message)
			daemon.knownVulnIDs = append(daemon.knownVulnIDs, vuln.id)
		}
	}
}

func (daemon *Daemon) sendEmail(subject, message *string) {
	const dateLayout = "Mon, 2 Jan 2006 15:04:05 -0700"
	body := "From: " + daemon.cfg.SMTP.From + "\r\nTo: " + daemon.cfg.SMTP.To +
		"\r\nSubject: " + *subject + "\r\nDate: " + time.Now().Format(dateLayout) +
		"\r\n\r\n" + *message
	domain, _, err := net.SplitHostPort(daemon.cfg.SMTP.Host)
	if err != nil {
		log.Critical("Error with net.SplitHostPort: %v", err)
	}
	auth := smtp.PlainAuth("", daemon.cfg.SMTP.User, daemon.cfg.SMTP.Pass, domain)
	err = smtp.SendMail(daemon.cfg.SMTP.Host, auth, daemon.cfg.SMTP.From,
		strings.Fields(daemon.cfg.SMTP.To), []byte(body))
	if err != nil {
		log.Critical("Error with smtp.SendMail: %v\n\n", err)
		log.Critical("Body: %v\n\n", body)
	}
}

// remove all cron daemon entries
func (daemon *Daemon) RemoveCronEntries() {
	entries := daemon.cron.Entries()
	for _, entry := range entries {
		log.Debug("Removing cron entry id %v", entry.ID)
		daemon.cron.Remove(entry.ID)
	}
}

// watch modules.yml file and host.d dir and recreate all cron entries upon changes
func (daemon *Daemon) CreateWatchers() {
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Fatal(err)
		}
		defer watcher.Close()

		done := make(chan bool) // block until emptied
		timer := time.NewTimer(0 * time.Second)
		<-timer.C //empty the channel
		var event string

		go func() {
			for {
				select {
				case evnt := <-watcher.Events:
					// TODO grep by whole dir name
					regex := regexp.MustCompilePOSIX(".*.yml$")
					if regex.MatchString(evnt.Name) {
						timer.Reset(3 * time.Second)
						log.Debug("Reset timer for event: %v", evnt)
						event = evnt.Name
					} else {
						log.Debug(fmt.Sprintf("Skipping file event: %s", evnt.Name))
					}
				case err := <-watcher.Errors:
					message := fmt.Sprintf("Error with file watcher:\n%v", err)
					log.Critical(message)
					daemon.errorChan <- message
				}
			}
		}()

		go func() {
			for {
				select {
				case <-timer.C:
					log.Info("Reloading configuration after %v write", event)
					daemon.RemoveCronEntries()
					daemon.LoadServicesYaml()
					daemon.LoadHostYamls()
					daemon.CreateCronEntries()
				}
			}
		}()

		err = watcher.Add(daemon.cfg.Sploit.WatchDir)
		if err != nil {
			log.Fatal(err)
		}
		err = watcher.Add(path.Dir(daemon.cfg.Sploit.ServicesFile))
		if err != nil {
			log.Fatal(err)
		}
		<-done
	}()
}

// serve html
func (daemon *Daemon) CreateWebserver() {
	authenticator := daemon.loadDigestAuth("Sploit")
	http.HandleFunc("/", auth.JustCheck(&authenticator, daemon.rootHandler))
	go func() {
		http.ListenAndServe(daemon.cfg.Sploit.ServeAddress, nil)
	}()
	log.Info("Started webserver on %v", daemon.cfg.Sploit.ServeAddress)
}

func (daemon *Daemon) rootHandler(writer http.ResponseWriter, request *http.Request) {
	var data struct {
		CronEntries []cron.Entry
		Vulns       []vulnerability
	}
	data.CronEntries = daemon.cron.Entries()
	vulns, err := daemon.selectVulns()
	if err != nil {
		message := fmt.Sprintf("Error selecting vulns from the database:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
	}
	data.Vulns = *vulns
	tmpl := template.Must(template.ParseFiles("root.html"))
	tmpl.Execute(writer, &data)
}

// run separate cron daemon to update msf at regular intervals
func (daemon *Daemon) CreateUpdaterNotifier() {
	daemon.internalCron = *cron.New()
	daemon.updateRunning = false
	_, err := daemon.internalCron.AddFunc(daemon.cfg.Sploit.UpdateSpec, daemon.updateMsf)
	if err != nil {
		log.Fatal(err)
	}
	log.Info("Setup to msfupdate on this schedule '%v'", daemon.cfg.Sploit.UpdateSpec)
	daemon.internalCron.Start()
}

func (daemon *Daemon) updateMsf() {
	if daemon.updateRunning {
		log.Warning("An update of MSF is already running, not running again.")
		return
	}
	daemon.updateRunning = true
	log.Info("Beginning an update of MSF via Git.")

	commands := []string{
		"msfupdate",
	}
	responseData, err := daemon.createConsoleAndRun(commands)
	if err != nil {
		message := fmt.Sprintf("Error running commands in console:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	regex := regexp.MustCompilePOSIX("No updates available")
	// skip the rest if msfupdate results in "No updates available"
	if regex.MatchString(responseData) {
		log.Info("MSF is already up to date")
		daemon.updateRunning = false
		daemon.lastUpdate = time.Now()
		return
	}

	commands = []string{
		"reload_all",
	}
	_, err = daemon.createConsoleAndRun(commands)
	if err != nil {
		message := fmt.Sprintf("Error running commands in console:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}

	// search modules for keywords, keep track of how many results there are, notify if it changes
	var buffer bytes.Buffer
	buffer.WriteString("search ")
	for _, service := range *daemon.Services {
		buffer.WriteString(fmt.Sprintf("%s ", service.Name))
	}
	commands = []string{
		buffer.String(),
	}
	responseData, err = daemon.createConsoleAndRun(commands)
	if err != nil {
		message := fmt.Sprintf("Error running commands in console:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	for _, service := range *daemon.Services {
		newModules := []string{}
		regex := regexp.MustCompilePOSIX(fmt.Sprintf("^.*%s*.*$", service.Name))
		lines := regex.FindAllString(responseData, -1)
		for _, line := range lines {
			exists := false
			for _, knownModule := range daemon.knownModules[service.Name] {
				if line == knownModule {
					exists = true
				}
			}
			if !exists {
				newModules = append(newModules, line)
			}
		}
		daemon.knownModules[service.Name] = lines
		if len(newModules) != 0 {
			var subject = fmt.Sprintf("New MSF modules matching '%s' found", service.Name)
			var message = fmt.Sprintf("After updating MSF, found the following new modules with '%s' in the name/description:\t\n%s",
				service.Name, strings.Join(newModules, "\n"),
			)
			daemon.sendEmail(&subject, &message)
		} else {
			log.Info("No new modules matching '%s' were found.", service.Name)
		}
	}

	daemon.updateRunning = false
	daemon.lastUpdate = time.Now()
}

// create console and run commands
func (daemon *Daemon) createConsoleAndRun(commands []string) (string, error) {
	var buffer bytes.Buffer
	console, err := daemon.API.ConsoleCreate()
	if err != nil {
		message := fmt.Sprintf("Error with ConsoleCreate:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return "", err
	}
	log.Debug("New console allocated: %v", console)

	_, err = daemon.API.ConsoleRead(console.ID)
	if err != nil {
		message := fmt.Sprintf("Error with ConsoleRead:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return "", err
	}
	log.Debug("Discarded console banner.")

	for _, command := range commands {
		command = fmt.Sprintf("%s\n", command)
		err = daemon.API.ConsoleWrite(console.ID, command)
		if err != nil {
			message := fmt.Sprintf("Error with ConsoleWrite:\n%v", err)
			log.Critical(message)
			daemon.errorChan <- message
			return "", err
		}
		log.Debug("Wrote '%#v' to console %v", command, console.ID)
		// don't read too soon or you get blank response
		time.Sleep(750 * time.Millisecond)
		busy := true
		for busy {
			response, err := daemon.API.ConsoleRead(console.ID)
			if err != nil {
				message := fmt.Sprintf("Error with ConsoleRead:\n%v", err)
				log.Critical(message)
				daemon.errorChan <- message
				return "", err
			}
			if response.Data != "" {
				log.Debug("Read console %v output:\n%v", console.ID, response.Data)
			}
			buffer.WriteString(response.Data)
			if response.Busy {
				log.Debug("Console %v is still busy, sleeping 3 seconds..", console.ID)
				time.Sleep(3 * time.Second)
			} else {
				busy = false
			}
		}
	}

	err = daemon.API.ConsoleDestroy(console.ID)
	if err != nil {
		message := fmt.Sprintf("Error with ConsoleDestroy:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return "", err
	}
	log.Debug("Successfully removed console %v", console)
	return buffer.String(), nil
}

// regular status/update email
func (daemon *Daemon) CreateStatusNotifier() {
	_, err := daemon.internalCron.AddFunc(daemon.cfg.Sploit.StatusSpec, daemon.sendStatusEmail)
	if err != nil {
		log.Fatal(err)
	}
}

func (daemon *Daemon) sendStatusEmail() {
	var subject = "MSF regular Status Update"
	var message bytes.Buffer
	// last successful MSF update
	when := "[An update has not yet been run]"
	if !daemon.lastUpdate.IsZero() {
		when = daemon.lastUpdate.String()
	}
	message.WriteString(fmt.Sprintf("Last successful MSF update: %v\n\n", when))
	// number of scans since last status email
	message.WriteString(fmt.Sprintf("%d scans have been run since the last email update\n\n", daemon.scanCount))
	// known vulns
	vulns, err := daemon.selectVulns()
	if err != nil {
		message := fmt.Sprintf("Error selecting vulns from the database:\n%v", err)
		log.Critical(message)
		daemon.errorChan <- message
		return
	}
	if len(*vulns) > 0 {
		message.WriteString("Currently know vulnerabilities: (Notifications have previously been sent.)\n\n")
		for _, vuln := range *vulns {
			str := fmt.Sprintf("\t%v %v %v %v\n", vuln.CreatedAt, vuln.Address, vuln.Name, vuln.References)
			message.WriteString(str)
		}
	} else {
		message.WriteString("No currently know vulnerabilities\n")
	}
	msg := message.String()
	daemon.sendEmail(&subject, &msg)
	daemon.scanCount = 0
	log.Debug("Sent Status Email")
}

func createAndLoadFile(name string) *os.File {
	if _, err := os.Stat(name); os.IsNotExist(err) {
		file, err := os.Create(name)
		if err != nil {
			log.Fatalf("Error with os.Create(): %v", err)
		}
		log.Debug("Created file %v", name)
		return file
	} else {
		file, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0640)
		if err != nil {
			log.Fatalf("Error with os.Open(): %v", err)
		}
		log.Debug("Opened existing file %v", name)
		return file
	}
}

func (daemon *Daemon) CreateErrorEmailer() {
	daemon.errorChan = make(chan string, 10)
	go func() {
		for message := range daemon.errorChan {
			var subject = "An error occured with Sploit:"
			daemon.sendEmail(&subject, &message)
		}
	}()
}

var log = logging.MustGetLogger("sploit")

func main() {
	// Do it already
	daemon := &Daemon{}
	daemon.knownModules = map[string][]string{}
	daemon.LoadFlags()
	daemon.LoadSploitYaml()
	daemon.SetupLogging()
	log.Info("Starting Sploitinator")
	daemon.waitGroup = *new(sync.WaitGroup) // supply a mechanism for staying running
	daemon.CreateInterruptChannel()
	daemon.CreateErrorEmailer()
	daemon.LoadServicesYaml()
	daemon.LoadHostYamls()
	daemon.OpenDBConnection()
	address := fmt.Sprintf("http://%s:%s%s", daemon.cfg.MsfRpc.Host, daemon.cfg.MsfRpc.Port, daemon.cfg.MsfRpc.URI)
	daemon.API = msfapi.New(address)
	daemon.SetupAPIToken()
	daemon.CreateNotifier()
	daemon.cron = *cron.New()
	daemon.CreateCronEntries()
	daemon.cron.Start()
	daemon.CreateWatchers()
	daemon.CreateWebserver()
	daemon.CreateUpdaterNotifier()
	daemon.CreateStatusNotifier()
	daemon.waitGroup.Wait() // Stay running until wg.Done() is called
}
