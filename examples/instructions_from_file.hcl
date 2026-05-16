#!/usr/bin/env docker agent run

agent "root" {
  description = "Agent that loads its system prompt from a separate file"
  model       = "auto"

  # The file() helper reads a text file and injects its contents here.
  # Relative paths are resolved from this HCL file's directory.
  instruction = file("instructions_from_file.md")

  welcome_message = "Hi! My instructions were loaded from instructions_from_file.md"
}
