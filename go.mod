module main

go 1.16

replace github.com/gogo/protobuf => github.com/regen-network/protobuf v1.3.3-alpha.regen.1

replace google.golang.org/grpc => google.golang.org/grpc v1.33.2

require (
	github.com/cosmos/cosmos-sdk v0.42.4
	github.com/google/uuid v1.3.0
	github.com/prometheus/client_golang v1.12.2
	github.com/rs/zerolog v1.27.0
	github.com/spf13/cobra v1.4.0
	github.com/spf13/pflag v1.0.5
	github.com/spf13/viper v1.12.0
	github.com/tendermint/tendermint v0.37.0-dev
	google.golang.org/grpc v1.50.1
)

replace (
	github.com/CosmWasm/wasmd => github.com/sei-protocol/sei-wasmd v0.0.1
	github.com/cosmos/cosmos-sdk => github.com/sei-protocol/sei-cosmos v0.2.26
	github.com/cosmos/iavl => github.com/sei-protocol/sei-iavl v0.1.3
	github.com/tendermint/tendermint => github.com/sei-protocol/sei-tendermint v0.2.14-0.20230428210900-48f6e69b9bf8
)
