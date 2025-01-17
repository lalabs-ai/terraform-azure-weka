variable "get_weka_io_token" {
  type        = string
  sensitive   = true
  description = "Get get.weka.io token for downloading weka"
}

variable "subscription_id" {
  type        = string
  description = "Subscription id for deployment"
}

variable "client_id" {
  type        = string
  description = "Client id of Service principal user"
}

variable "tenant_id" {
  type        = string
  description = "Tenant id"
}

variable "client_secret" {
  type        = string
  description = "Password of service principal user"
}

variable "rg_name" {
  type = string
  description = "Resource group name"
}

variable "prefix" {
  type = string
  description = "Prefix for all resources"
}

variable "cluster_name" {
  type = string
  description = "Name of the cluster"
}

variable "cluster_size" {
  type = number
  description = "Number of nodes in the cluster"
}
