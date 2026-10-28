# Name the Adoption's Repository in Its Facts

Mandatory note: a contract change (#2407).

Chose to serve the adoption's repository from the card's project's current
record, as the filing facts do, rather than copying the name from the
upkeep refusal. The project binds the repository's forge ID, while its name
may change after the refusal. The card must show the current name without
changing what an approval adopts.

Chose a shared `ProjectRepository` wire schema for filing and adoption facts
over two identical schemas. Its JSON shape stays `repo` and `repository_id`;
the existing Go domain type keeps its name. The adoption's repository is
required, so a client cannot decode incomplete facts and offer an unnamed
adoption for approval.

The schema now describes the approval that revision 101 already implements:
the daemon records the adoption for the card's repository directly. This
change adds display facts, not a new approval or policy gate.

## Refute-First Checks

- The service reads the repository only after the existing store gate ties
  the item to the proposal's project and rechecks the refusal. The added
  field supplies no authority to the approval path.
- The served-facts regression checks both the original repository identity
  and its current name after a project rename. The filing repository mapping
  and required contract digest are checked alongside it.
- A fresh-context review found no reachable correctness or security defect
  in the schema, generated client, service mapping, or display changes.
