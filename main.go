package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/common/log"

	"crypto/md5"
	tmpltext "text/template"
)

var (
	configFile           = kingpin.Flag("config.file", "ServiceNow configuration file.").Default("config/servicenow.yml").String()
	listenAddress        = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9877").String()
	config               Config
	serviceNow           ServiceNow
	noUpdateStates       map[json.Number]bool
	incidentUpdateFields map[string]bool

	webhookRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_requests_total",
			Help: "Total number of HTTP requests on /webhook.",
		},
		[]string{"code"},
	)

	webhookLastRequest = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "webhook_last_request_time_seconds",
			Help: "Unix/epoch time of the last HTTP request on /webhook.",
		},
	)

	webhookIncidentValidationError = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_incident_validation_errors_total",
			Help: "Total number of incident validation errors.",
		},
	)

	webhookIncidentTemplateError = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_incident_template_errors_total",
			Help: "Total number of incident template errors.",
		},
	)

	serviceNowRequests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "servicenow_requests_total",
			Help: "Total number of HTTP requests to ServiceNow instance.",
		},
		[]string{"host", "method", "code"},
	)

	serviceNowLastRequest = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "servicenow_last_request_time_seconds",
			Help: "Unix/epoch time of the last HTTP request to ServiceNow instance.",
		},
	)

	serviceNowError = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "servicenow_errors_total",
			Help: "Total number of ServiceNow errors.",
		},
	)
)

// Config - ServiceNow webhook configuration
type Config struct {
	ServiceNow      ServiceNowConfig  `yaml:"service_now"`
	Workflow        WorkflowConfig    `yaml:"workflow"`
	DefaultIncident map[string]string `yaml:"default_incident"`
}

// ServiceNowConfig - ServiceNow instance configuration
type ServiceNowConfig struct {
	InstanceName string `yaml:"instance_name"`
	UserName     string `yaml:"user_name"`
	Password     string `yaml:"password"`
}

// WorkflowConfig - Incident workflow configuration
type WorkflowConfig struct {
	IncidentGroupKeyField string        `yaml:"incident_group_key_field"`
	NoUpdateStates        []json.Number `yaml:"no_update_states"`
	IncidentUpdateFields  []string      `yaml:"incident_update_fields"`
}

// JSONResponse is the Webhook http response
type JSONResponse struct {
	Status  int
	Message string
}

func init() {
	prometheus.MustRegister(version.NewCollector("alertmanager_webhook_servicenow"))
}

func (c Config) validate() error {
	var errs strings.Builder

	if len(c.ServiceNow.InstanceName) == 0 {
		errs.WriteString("instance_name is missing\n")
	}
	if len(c.ServiceNow.UserName) == 0 {
		errs.WriteString("user_name is missing\n")
	}
	if len(c.ServiceNow.Password) == 0 {
		errs.WriteString("password is missing\n")
	}
	if len(c.Workflow.IncidentGroupKeyField) == 0 {
		errs.WriteString("incident_group_key_field is missing\n")
	}

	if errs.Len() > 0 {
		return errors.New("Config file is invalid\n" + errs.String())
	}
	return nil
}

func webhook(w http.ResponseWriter, r *http.Request) {

	data, err := readRequestBody(r)
	if err != nil {
		log.Errorf("Error reading request body : %v", err)
		sendJSONResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	// Need to raise an incident for each alert on service now
	newdata := make(map[int]template.Data, len(data.Alerts))
	for i, alert := range data.Alerts {
		newdata[i] = data
		newdata[i].Alerts[0] = alert

		err = onAlertGroup(newdata[i])

		if err != nil {
			log.Errorf("Error managing incident from alert : %v", err)
			sendJSONResponse(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	// err = onAlertGroup(data)

	// if err != nil {
	// 	log.Errorf("Error managing incident from alert : %v", err)
	// 	sendJSONResponse(w, http.StatusInternalServerError, err.Error())
	// 	return
	// }

	// Returns a 200 if everything went smoothly
	sendJSONResponse(w, http.StatusOK, "Success")
}

func homepage(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(`<html>
	<head><title>alertmanager-webhook-servicenow</title></head>
	<body>
	<h1>alertmanager-webhook-servicenow</h1>
	<p><a href="/metrics">Metrics</a></p>
	</body>
	</html>`))
}

// Starts the following http handler:
// - basic home page on /
// - Alertmanager webhook entry point on /webhook
// - health metrics on /metrics
func main() {
	kingpin.Version(version.Print("alertmanager-webhook-servicenow"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	_, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("Error loading config file: %v", err)
	}

	_, err = loadSnClient()
	if err != nil {
		log.Fatalf("Error loading ServiceNow client: %v", err)
	}

	log.Info("Starting webhook", version.Info())
	log.Info("Build context", version.BuildContext())

	http.HandleFunc("/", homepage)
	http.HandleFunc("/webhook", webhook)
	http.Handle("/metrics", promhttp.Handler())

	log.Infof("listening on: %v", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func sendJSONResponse(w http.ResponseWriter, status int, message string) {
	webhookRequests.WithLabelValues(strconv.Itoa(status)).Inc()
	webhookLastRequest.SetToCurrentTime()

	data := JSONResponse{
		Status:  status,
		Message: message,
	}
	bytes, _ := json.Marshal(data)

	w.WriteHeader(status)
	_, err := w.Write(bytes)

	if err != nil {
		log.Errorf("Error writing JSON response: %s", err)
	}
}

func readRequestBody(r *http.Request) (template.Data, error) {

	// Do not forget to close the body at the end
	defer r.Body.Close()

	// Extract data from the body in the Data template provided by AlertManager
	data := template.Data{}
	err := json.NewDecoder(r.Body).Decode(&data)

	return data, err
}

func loadConfigContent(configData []byte) (Config, error) {
	config = Config{}
	var err error

	err = yaml.Unmarshal([]byte(configData), &config)
	if err != nil {
		return config, err
	}

	loadEnvVars(&config)

	err = config.validate()
	if err != nil {
		return config, err
	}

	// Load internal state from config
	noUpdateStates = make(map[json.Number]bool, len(config.Workflow.NoUpdateStates))
	for _, s := range config.Workflow.NoUpdateStates {
		noUpdateStates[s] = true
	}

	// Load internal incidents update fields from config
	incidentUpdateFields = make(map[string]bool, len(config.Workflow.IncidentUpdateFields))
	for _, f := range config.Workflow.IncidentUpdateFields {
		incidentUpdateFields[f] = true
	}
	log.Info("ServiceNow config loaded")
	return config, nil
}

func loadConfig(configFile string) (Config, error) {
	// Load the config from the file
	configData, err := ioutil.ReadFile(configFile)
	if err != nil {
		return Config{}, err
	}

	return loadConfigContent(configData)
}

func loadEnvVars(c *Config) {
	if instanceName, ok := os.LookupEnv("SERVICENOW_INSTANCE_NAME"); ok {
		(*c).ServiceNow.InstanceName = instanceName
	}
	if userName, ok := os.LookupEnv("SERVICENOW_USERNAME"); ok {
		(*c).ServiceNow.UserName = userName
	}
	if password, ok := os.LookupEnv("SERVICENOW_PASSWORD"); ok {
		(*c).ServiceNow.Password = password
	}
	if incidentField, ok := os.LookupEnv("SERVICENOW_INCIDENT_GROUP_KEY_FIELD"); ok {
		(*c).Workflow.IncidentGroupKeyField = incidentField
	}
}

func loadSnClient() (ServiceNow, error) {
	var err error
	serviceNow, err = NewServiceNowClient(config.ServiceNow.InstanceName, config.ServiceNow.UserName, config.ServiceNow.Password)
	if err != nil {
		return serviceNow, err
	}
	return serviceNow, nil
}

func onAlertGroup(data template.Data) error {
	log.Infof("Received alert: Status=%s, Labels=%v, Annotations=%v",
		data.Alerts[0].Status, data.Alerts[0].Labels, data.Alerts[0].Annotations)

	getParams := map[string]string{
		config.Workflow.IncidentGroupKeyField: getGroupKey(data),
	}

	existingIncidents, err := serviceNow.GetIncidents(getParams)
	if err != nil {
		serviceNowError.Inc()
		return err
	}
	log.Infof("Found %v existing incident(s) for alert group key: %s.", len(existingIncidents), getGroupKey(data))

	updatableIncidents := filterUpdatableIncidents(existingIncidents)
	log.Infof("Found %v updatable incident(s) for alert group key: %s.", len(updatableIncidents), getGroupKey(data))

	var updatableIncident Incident
	if len(updatableIncidents) > 0 {
		updatableIncident = updatableIncidents[0]

		if len(updatableIncidents) > 1 {
			log.Warnf("As multiple updable incidents were found for alert group key: %s, first one will be used: %s", getGroupKey(data), updatableIncident.GetNumber())
		}
	}

	if data.Alerts[0].Status == "firing" {
		return onFiringGroup(data, updatableIncident)
	} else if data.Alerts[0].Status == "resolved" {
		return onResolvedGroup(data, updatableIncident)
	} else {
		log.Errorf("Unknown alert group status: %s", data.Alerts[0].Status)
	}

	return nil
}

func onFiringGroup(data template.Data, updatableIncident Incident) error {
	incidentCreateParam, err := alertGroupToIncident(data)
	if err != nil {
		return err
	}

	incidentUpdateParam := filterForUpdate(incidentCreateParam)

	if updatableIncident == nil {
		log.Infof("Found no updatable incident for firing alert group key: %s", getGroupKey(data))
		if _, err := serviceNow.CreateIncident(incidentCreateParam); err != nil {
			serviceNowError.Inc()
			return err
		}
	} else {
		log.Infof("Found updatable incident (%s), with state %s, for firing alert group key: %s", updatableIncident.GetNumber(), updatableIncident.GetState(), getGroupKey(data))
		if _, err := serviceNow.UpdateIncident(incidentUpdateParam, updatableIncident.GetSysID()); err != nil {
			serviceNowError.Inc()
			return err
		}
	}
	return nil
}

func onResolvedGroup(data template.Data, updatableIncident Incident) error {
	incidentCreateParam, err := alertGroupToIncident(data)
	if err != nil {
		return err
	}

	incidentUpdateParam := filterForUpdate(incidentCreateParam)
	incidentUpdateParam["u_state"] = "2"                                // set to in-progress - API does not allow us to change to resolved
	incidentUpdateParam["u_work_notes"] = "Incident has been resolved." // add this work note message

	if updatableIncident == nil {
		log.Infof("Found no updatable incident for resolved alert group key: %s. No incident will be created/updated.", getGroupKey(data))
	} else {
		log.Infof("Found updatable incident (%s), with state %s, for resolved alert group key: %s", updatableIncident.GetNumber(), updatableIncident.GetState(), getGroupKey(data))
		if _, err := serviceNow.UpdateIncident(incidentUpdateParam, updatableIncident.GetSysID()); err != nil {
			serviceNowError.Inc()
			return err
		}
	}
	return nil
}

func alertGroupToIncident(data template.Data) (Incident, error) {

	incident := Incident{
		"caller_id":                           config.ServiceNow.UserName,
		config.Workflow.IncidentGroupKeyField: getGroupKey(data),
	}

	for k, v := range config.DefaultIncident {
		incident[k] = v
	}

	applyIncidentTemplate(incident, data)
	err := validateIncident(incident)
	if err != nil {
		webhookIncidentValidationError.Inc()
		log.Error(err)
	}
	return incident, nil
}

func filterForUpdate(incident Incident) Incident {
	incidentUpdate := Incident{}
	for field, value := range incident {
		if incidentUpdateFields[field] {
			incidentUpdate[field] = value
		}
	}
	incidentUpdate["u_function"] = "Update"
	return incidentUpdate
}

func filterUpdatableIncidents(incidents []Incident) []Incident {
	var updatableIncidents []Incident
	for _, incident := range incidents {
		if !noUpdateStates[incident.GetState()] {
			updatableIncidents = append(updatableIncidents, incident)
		}
	}
	return updatableIncidents
}

func getGroupKey(data template.Data) string {
	// Use fingerprints as the group key instead
	// only use fingerprint of first alert
	hash := md5.Sum([]byte(fmt.Sprintf("%vT1", data.Alerts[0].Fingerprint))) // added a T1 for testing for now as these things are bloody unique in service now
	return fmt.Sprintf("%x", hash)
}

func applyIncidentTemplate(incident Incident, data template.Data) {
	for key, val := range incident {
		var err error
		incident[key], err = applyTemplate(key, val.(string), data)
		if err != nil {
			webhookIncidentTemplateError.Inc()
			log.Errorf("Error parsing default incident template for key:%s value:%s, error:%v", key, val.(string), err)
		}
	}
}

func applyTemplate(name string, text string, data template.Data) (string, error) {
	tmpl, err := tmpltext.New(name).Parse(text)
	if err != nil {
		return "", err
	}

	var result bytes.Buffer
	err = tmpl.Execute(&result, data)
	if err != nil {
		return "", err
	}

	return result.String(), nil
}

func validateIncident(incident Incident) error {
	var str strings.Builder
	if impact, ok := incident["impact"]; ok && impact != nil && len(impact.(string)) > 0 {
		if _, err := strconv.Atoi(impact.(string)); err != nil {
			str.WriteString("'impact' field value is ")
			str.WriteString(impact.(string))
			str.WriteString(" but should be an integer, please fix your configuration. Incident creation/update will proceed but this field will be missing. ")
		}
	}

	if urgency, ok := incident["urgency"]; ok && urgency != nil && len(urgency.(string)) > 0 {
		if _, err := strconv.Atoi(urgency.(string)); err != nil {
			str.WriteString("'urgency' field value is ")
			str.WriteString(urgency.(string))
			str.WriteString(" but should be an integer, please fix your configuration. Incident creation/update will proceed but this field will be missing.")
		}
	}
	if str.Len() > 0 {
		return errors.New(str.String())
	}
	return nil
}
