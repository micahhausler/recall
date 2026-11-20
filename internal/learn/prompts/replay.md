# Replay one moment

You are re-running one moment of a coding agent's past session, as the agent.
Your input, after this prompt, has three sections: RULES (the context rules the agent has loaded, possibly none), MISTAKE (what went wrong at this point in the original session, with the owner's words when the owner corrected it), and SESSION (the compressed transcript up to the moment, ending at the owner's last prompt; `[owner]` messages are preserved in full, the rest is mechanically compressed).

Decide what you, as the agent, would do next in answer to that last prompt, following the RULES, and judge that one next action against the MISTAKE.
Decide the action first, from the rules and the session alone; the MISTAKE section is the yardstick for the verdict, not an instruction to follow.

Output exactly two lines, nothing else:

Next: <the next action, concretely, in one sentence>
Verdict: repeats | avoids

`repeats` when the next action makes the described mistake again; `avoids` when it does not.
A line in any other shape is discarded, and a missing Verdict line fails the replay.
