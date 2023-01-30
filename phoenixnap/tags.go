package phoenixnap

import (
	"context"
	"fmt"

	"github.com/phoenixnap/go-sdk-bmc/tagapi"
)

// ensureTags ensure that the given tags exist.
// In PhoenixNAP cloud, tag names must exist separately as a resource
// before they can be assigned to a resource like a server or IP block.
func ensureTags(client *tagapi.APIClient, tags ...string) error {
	// rather than trying to create all of them and erroring,
	// we will get all of the tags that exist already, and find the ones we need
	retTags, _, err := client.TagsApi.TagsGet(context.Background()).Execute()
	if err != nil {
		return fmt.Errorf("unable to get all tags: %w", err)
	}
	foundTags := make(map[string]bool)
	for _, tag := range retTags {
		foundTags[tag.Name] = true
	}
	var toCreate []string
	for _, tag := range tags {
		if !foundTags[tag] {
			toCreate = append(toCreate, tag)
		}
	}
	// no tags to create, they all already exist
	for _, tag := range toCreate {
		tagCreate := tagapi.NewTagCreate(tag, false)
		if _, _, err := client.TagsApi.TagsPost(context.Background()).TagCreate(*tagCreate).Execute(); err != nil {
			return fmt.Errorf("unable to create tag %s: %w", tag, err)
		}
	}
	return nil
}
