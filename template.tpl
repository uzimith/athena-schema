-- Code generated by "{{.CmdLog}}"; DO NOT EDIT
{{range $_, $table := .Tables}}
CREATE EXTERNAL TABLE IF NOT EXISTS `{{.TableName}}` (
{{range $index, $column := .Columns}}  `{{.Name}}` {{.Type}}{{if not (last $index $table.Columns)}},{{end}}
{{end}}) PARTITIONED BY (
  `year` int,
  `month` int,
  `day` int
)
ROW FORMAT SERDE 'org.openx.data.jsonserde.JsonSerDe'
LOCATION 's3://{{$.BucketName}}/{{$.FolderNamePrefix}}{{.FolderName}}{{$.FolderNameSuffix}}'
;{{end}}
