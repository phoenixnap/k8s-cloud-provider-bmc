package phoenixnap

import "github.com/phoenixnap/go-sdk-bmc/ipapi"

// utility methods
func tagAssignmentsIntoRequests(tags []ipapi.TagAssignment) []ipapi.TagAssignmentRequest {
	requests := []ipapi.TagAssignmentRequest{}
	for _, tag := range tags {
		requests = append(requests, ipapi.TagAssignmentRequest{
			Name:  tag.Name,
			Value: tag.Value,
		})
	}
	return requests
}
