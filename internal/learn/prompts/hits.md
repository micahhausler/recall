# Rule hits

The input ends with a CURRENT RULES section listing the owner's standing rules, each with an id.
Report one line for each rule the session touched, and nothing for a rule it never touched:

- **relevant**: the situation the rule covers came up.
- **followed**: the output did what the rule says.
- **violated**: the output broke the rule, or the owner had to restate it.

Rule[<id>]: relevant|followed|violated — <one short note>

Use only ids from CURRENT RULES.
