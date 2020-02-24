package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"
)

const (
	asgCheckDelay = 30 // Default delay between checks of ASG status in seconds
)

var (
	verbose = os.Getenv("VERBOSE") == "true"
)

func main() {
	// get a kube connection
	k8sSvc, err := createK8sService()
	if err != nil {
		log.Fatalf("Error getting kubernetes KubernetesService handler when required: %v", err)
	}

	// get the AWS sessions
	asgSvc, err := awsGetServices()
	if err != nil {
		log.Fatalf("Unable to create an AWS session: %v", err)
	}

	// to keep track of original target sizes during rolling updates
	detachingNodes := map[string]map[string]bool{}

	checkDelay, err := getDelay()
	if err != nil {
		log.Fatalf("Unable to get delay: %s", err.Error())
	}

	// infinite loop
	for {
		err := detachUnschedulables(asgSvc, k8sSvc, detachingNodes)
		if err != nil {
			log.Printf("Error adjusting AutoScaling Groups: %v", err)
		}
		// delay with each loop
		log.Printf("Sleeping %d seconds\n", checkDelay)
		time.Sleep(time.Duration(checkDelay) * time.Second)
	}
}

// Returns delay value to use in loop. Uses default if not defined.
func getDelay() (int, error) {
	delayOverride, exist := os.LookupEnv("ROLLER_CHECK_DELAY")
	if exist {
		delay, err := strconv.Atoi(delayOverride)
		if err != nil {
			return -1, fmt.Errorf("ROLLER_CHECK_DELAY is not parsable: %v (%s)", delayOverride, err.Error())
		}
		return delay, nil
	}

	return asgCheckDelay, nil
}
