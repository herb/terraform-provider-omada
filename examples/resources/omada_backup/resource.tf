# Automatic backup schedule (Global View -> Settings -> Backup & Restore).
#
# Controller-scoped: automatic backup belongs to the controller, not a site, so
# there is no `site` attribute and times are in the controller's own time zone
# (`omada_controller_settings.time_zone`).
#
# The selectors below are each tied to one frequency. Set only the one that
# applies.
resource "omada_backup" "weekly" {
  enabled     = true
  timing_type = 2 # 1 daily, 2 weekly, 3 monthly, 4 yearly
  day_of_week = 0 # 0 Sunday .. 6 Saturday (weekly only)
  hour        = 3 # 03:00, controller time
  minute      = 0

  # `retention` is retained-data HISTORY in days, not backup-file lifetime:
  # -1 settings only, 0 all history (disabled on hardware controllers),
  # otherwise 7/30/60/90/180/365 days of data.
  retention = 30
  # `max_files` separately caps how many backup files exist.
  max_files = 14

  # Content selection. Leaving one unset keeps the controller's current value
  # instead of overwriting it with false.
  retain_setting      = true
  retain_user         = false
  retain_auth_record  = true
  retain_firmware_log = true
}

# A monthly schedule selects a day of the month instead:
#
#   resource "omada_backup" "monthly" {
#     enabled      = true
#     timing_type  = 3
#     day_of_month = 1
#     hour         = 3
#     retention    = 30
#     max_files    = 14
#   }
#
# Disabling sends exactly {"enable": false}. Changing anything else in the same
# apply as a disable is refused — apply the change first, then disable.

# `terraform destroy` only forgets the schedule: automatic backups keep running
# and existing backup files are kept.
