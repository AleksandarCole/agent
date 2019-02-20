package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"

	handlers "github.com/gorilla/handlers"
	mux "github.com/gorilla/mux"

	api "github.com/semaphoreci/agent/pkg/api"
	jobs "github.com/semaphoreci/agent/pkg/jobs"
)

type Server struct {
	Host    string
	Port    int
	State   string
	Version string

	TLSKeyPath  string
	TLSCertPath string

	JwtSecret []byte

	Logfile io.Writer

	ActiveJob *jobs.Job
	router    *mux.Router
}

func NewServer(host string, port int, version string, logfile io.Writer, jwtSecret []byte) *Server {
	router := mux.NewRouter().StrictSlash(true)

	server := &Server{
		Host:        host,
		Port:        port,
		State:       "waiting for job",
		TLSKeyPath:  "server.key",
		TLSCertPath: "server.crt",
		JwtSecret:   jwtSecret,
		Logfile:     logfile,
		router:      router,
		Version:     version,
	}

	jwtMiddleware := CreateJwtMiddleware(jwtSecret)

	router.HandleFunc("/status", jwtMiddleware(server.Status)).Methods("GET")
	router.HandleFunc("/jobs", jwtMiddleware(server.Run)).Methods("POST")

	// The path /stop is the new standard, /jobs/terminate is here to support the legacy system.
	router.HandleFunc("/stop", jwtMiddleware(server.Stop)).Methods("POST")
	router.HandleFunc("/jobs/terminate", jwtMiddleware(server.Stop)).Methods("POST")

	// The path /jobs/{job_id}/log is here to support the legacy systems.
	router.HandleFunc("/job_logs", jwtMiddleware(server.JobLogs)).Methods("GET")
	router.HandleFunc("/jobs/{job_id}/log", jwtMiddleware(server.JobLogs)).Methods("GET")

	// Agent Logs
	router.HandleFunc("/agent_logs", jwtMiddleware(server.AgentLogs)).Methods("GET")

	return server
}

func (s *Server) Serve() {
	address := fmt.Sprintf("%s:%d", s.Host, s.Port)

	fmt.Printf("Agent %s listening on https://%s\n", s.Version, address)

	loggedRouter := handlers.LoggingHandler(s.Logfile, s.router)

	log.Fatal(http.ListenAndServeTLS(
		address,
		s.TLSCertPath,
		s.TLSKeyPath,
		loggedRouter,
	))
}

func (s *Server) Status(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(400)
	m := make(map[string]interface{})

	m["state"] = s.State
	m["version"] = s.Version

	jsonString, _ := json.Marshal(m)

	fmt.Fprintf(w, string(jsonString))
}

func (s *Server) JobLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "text/plain")

	startFromLine, err := strconv.Atoi(r.URL.Query().Get("start_from"))
	if err != nil {
		startFromLine = 0
	}

	logfile, err := os.Open("/tmp/job_log.json")

	if err != nil {
		w.WriteHeader(404)
		return
	}
	defer logfile.Close()

	logLine := 0
	scanner := bufio.NewScanner(logfile)
	for scanner.Scan() {
		if logLine >= startFromLine {
			fmt.Fprintln(w, scanner.Text())
		}

		logLine += 1
	}

	if r.Header.Get("X-Client-Name") == "archivator" {
		s.ActiveJob.JobLogArchived = true
	}
}

func (s *Server) AgentLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "text/plain")

	logfile, err := os.Open("/tmp/agent_log")

	if err != nil {
		w.WriteHeader(404)
		return
	}
	defer logfile.Close()

	io.Copy(w, logfile)
}

func (s *Server) Run(w http.ResponseWriter, r *http.Request) {
	if s.State != "waiting for job" {
		w.WriteHeader(422)
		fmt.Fprintf(w, `{"message": "a job is already running"}`)
		return
	}

	s.State = "received-job"

	body, err := ioutil.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	request, err := api.NewRequestFromJSON(body)

	if err != nil {
		fmt.Fprintf(w, `{"message": "%s"}`, err)
		return
	}

	job, err := jobs.NewJob(request)

	if err != nil {
		fmt.Fprintf(w, `{"message": "%s"}`, err)
		return
	}

	s.ActiveJob = job
	go s.ActiveJob.Run()

	s.State = "job-started"
}

func (s *Server) Stop(w http.ResponseWriter, r *http.Request) {
	go s.ActiveJob.Stop()

	w.WriteHeader(200)
}

func (s *Server) unsuported(w http.ResponseWriter) {
	w.WriteHeader(400)
	fmt.Fprintf(w, `{"message": "not supported"}`)
}
