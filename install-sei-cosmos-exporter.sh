#!/usr/bin/env bash

BOND_DENOM=usei
BENCH_PREFIX=sei

echo -e "\e[1m\e[32m1. Installing cosmos-exporter... \e[0m" && sleep 1
source ~/.bashrc

# install cosmos-exporter
sudo /usr/local/go/bin/go build -o build/sei-cosmos-exporter -buildvcs=false ./
sudo /usr/bin/go build -o build/sei-cosmos-exporter -buildvcs=false ./
sudo go build -o build/sei-cosmos-exporter -buildvcs=false ./
sudo mv ./build/sei-cosmos-exporter /usr/bin

sudo useradd -rs /bin/false cosmos_exporter

sudo tee <<EOF >/dev/null /etc/systemd/system/cosmos-exporter.service
[Unit]
Description=Cosmos Exporter
After=network-online.target

[Service]
User=cosmos_exporter
Group=cosmos_exporter
TimeoutStartSec=0
CPUWeight=95
IOWeight=95
ExecStartPre=/bin/bash -c 'timeout 300 bash -c "until curl -sf http://localhost:26657/status > /dev/null 2>&1; do sleep 5; done"'
ExecStart=/usr/bin/sei-cosmos-exporter --denom $BOND_DENOM --denom-coefficient 1000000 --bech-prefix $BENCH_PREFIX
Restart=always
RestartSec=10
LimitNOFILE=800000
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable cosmos-exporter
sudo systemctl restart cosmos-exporter

# Health check cron: restart cosmos-exporter if /metrics/general is unreachable
(sudo crontab -l 2>/dev/null | grep -v 'metrics/general.*cosmos-exporter'; echo '*/5 * * * * curl -sf --max-time 5 http://localhost:9300/metrics/general > /dev/null || { logger -t cosmos-exporter-healthcheck "metrics unreachable, restarting"; systemctl restart cosmos-exporter; }') | sudo crontab -

echo "installed"
