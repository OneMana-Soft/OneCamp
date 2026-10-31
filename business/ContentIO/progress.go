package business

import (
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// publishJob tells the job's owner's open tabs where it stands, on their own
// activity topic. Best effort: GET /content-io/jobs/{id} always answers.
var publishJob = func(j Job) {
	mqttBusiness.PublishMessageToUser(j.UserUUID, mqttStruct.MESSAGE_CONTENT_IO_JOB, jobMessage(j))
}

// jobMessage is a job as its message carries it: without a ready job's
// preview, which can be larger than a message may be, as the tabs read the
// job for it. Pure.
func jobMessage(j Job) Job {
	j.Preview = nil
	return j
}
