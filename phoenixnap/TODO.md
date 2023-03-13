# TODO

* decide if the IP block will be /29 or /30 or /31, and how to use the addresses
* decide if the IP address will be the first, second or third address in the block, or maybe broadcast
* create the block, assign it to the public network
* have the user pass the public network as a parameter pnap-l2://network-id
* user must have host configured with the right link address
* when delete service, create a thread to:
  1. remove tags, adding tags that show it is ready for unassignment and deletion
  2. unassign
  3. delete the block