extends Node
# Comfortable Homes / formerly HouseHelper. Local settings only.
var enabled := true

func save_settings():
	var file = FileAccess.open("user://comfortable-homes-settings.json", FileAccess.WRITE)
	if file:
		file.store_string(JSON.stringify({"enabled": enabled}))

func friendly_greeting() -> String:
	return "Welcome home!"
