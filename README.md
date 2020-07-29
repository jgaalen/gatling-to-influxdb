# gatling-to-influxdb

## Usage

To get help on application usage use `--help` (`-h`) key. It will provide all existing keys and simple examples.

`g2i` needs to be started before gatling test. For that a detached mode is available using `-d` key. It will print PID of started process so it can be later interrupted or killed. Also application log is written to `g2i.log` file so any issues with application can be traced there.

By default `g2i` looks for InfluxDB at `http://localhost:8086` but it can be easily changed using `-a` key with another HTTP address.

Default database name is `gatling`, it can be changed using `--database` (`-b`) key following another name.

If database uses authentication credentials can be provided using `--username` and `--password` (`-u`, `-p` respectfully) keys.

Integrating to CI can be done by running a set of commands like this (example uses SBT):

```bash
echo "Cleaning old stuff" && \
sbt clean && \
echo "Starting g2i in detached mode, saving PID in variable" && \
G2IPID=$(g2i ./target/gatling -a http://localhost:8086 -u root -p root -b gatling -t "MyUniqueTestId" -d | awk '{print $2}') && \
echo "Compile and launch simulation" && \
sbt compile "gatling:testOnly simulations.MyCoolSimulation"; \
echo "Sending interrupt signal to g2i process" && \
kill -INT $G2IPID && \
echo "Waiting for process to stop..." && \
sleep 10 && \
echo "Exiting"
```

## Warning

Application can be used for parsing an existing log file, but users info will be corrupted, so use at your own risk. Right behavior for this case swill be implemented later (probably).

No unit tests are written for application as of right now, indicating it's absolutely not production ready and battle tested. But you can try it anyway :)

## Building application

For building go version 1.11 or older is required (though it was built with version 1.13) with enabled modules support `GO111MODULE=on`. GOBIN variable should be in path to run application after installing from anywhere.

```bash
git clone git@github.com:Dakaraj/gatling-to-influxdb.git
cd gatling-to-influxdb
go mod download
go install g2i.go
```
