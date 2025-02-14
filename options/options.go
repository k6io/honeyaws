package options

type Options struct {
	Dataset              string  `short:"d" long:"dataset" description:"Name of the dataset" default:"aws-$SERVICE-access"`
	SampleRate           int     `long:"samplerate" description:"Only send 1 / N log lines" default:"1"`
	GrafanaCloudID       string  `long:"grafana_cloud_id" description:"Grafana Cloud user ID"`
	GrafanaCloudAPIKey   string  `long:"grafana_cloud_api_key" description:"Grafana Cloud API key"`
	GrafanaCloudEndpoint string  `long:"grafana_cloud_endpoint" description:"Grafana Cloud Loki endpoint"`
	Environment          string  `long:"environment" description:"Environment label" default:"dev"`
	StateDir             string  `long:"statedir" description:"Directory where ingest state is stored" default:"."`
	HighAvail            bool    `long:"highavail" description:"Enable high availability ingestion using DynamoDB"`
	BackfillHr           int     `long:"backfill" description:"The number of hours to increase backfill of log ingestion to with max of 168 hours (1 week)" default:"1"`
	EdgeMode             bool    `long:"edge_mode" description:"Ignore any parent trace id, if present, from a load balancer"`
	SamplerType          string  `long:"sampler_type" default:"simple" description:"Type of dynamic sampler to use. Options are 'simple' and 'ema'"`
	SamplerInterval      int     `long:"sampler_interval" default:"300" description:"Interval between sample rate calculation, in seconds."`
	SamplerDecay         float64 `long:"sampler_decay" default:"0.5" description:"Used only when sampler_type is set to 'ema'. A value between (0,1) that controls how fast new observations are factored into the moving average. Larger values mean the sample rates are more sensitive to recent observations."`

	Version bool   `short:"V" long:"version" description:"Show version"`
	APIHost string `hidden:"true" long:"api_host" description:"Host for the Honeycomb API" default:"https://api.honeycomb.io/"`
	Debug   bool   `long:"debug" description:"Print debugging output"`
}
